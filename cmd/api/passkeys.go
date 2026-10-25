package main

// Passkeys (WebAuthn): adding one to the signed-in account. Signing in with
// one is in passkeys_signin.go.
//
//	POST /me/passkeys/start  → options for the browser's navigator.credentials.create(),
//	                           with a fresh single-use challenge
//	POST /me/passkeys        → what the device made; checked, then the public key is stored
//
// No attestation ("none"): we don't ask the device to prove its make and
// model, only to make a key for this site after verifying the person (Face ID,
// fingerprint, device PIN). So nothing is signed yet at this step; what we
// check is that the answer belongs to our challenge, our site and our page.
//
// The browser hands over the public key ready-made (getPublicKey(): the
// standard DER form that x509 reads) and the authenticator data
// (getAuthenticatorData()), so we need no CBOR decoder and no dependency.

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	passkeyChallengeTTL = 5 * time.Minute // how long the browser has to answer
	maxPasskeys         = 10              // active passkeys per account
	coseES256           = -7              // ECDSA with P-256 and SHA-256: what phones and laptops use
	coseRS256           = -257            // RSA with SHA-256: some Windows Hello keys
)

// Bits in the authenticator data's flags byte (WebAuthn spec, §6.1).
const (
	flagUserPresent  = 0x01 // someone touched / looked at the device
	flagUserVerified = 0x04 // and proved it's them (Face ID, fingerprint, PIN)
	flagAttestedData = 0x40 // a new credential (ID + key) follows: registration only
)

// relyingParty is this site, as WebAuthn sees it: the host name passkeys
// belong to (rpID) and the exact origin pages are served from. Both come from
// the site's base URL, so each environment's passkeys are its own.
type relyingParty struct{ id, origin string }

func relyingPartyFor(base string) relyingParty {
	u, _ := url.Parse(base)
	return relyingParty{id: u.Hostname(), origin: u.Scheme + "://" + u.Host}
}

// b64 is base64url without padding, how WebAuthn writes bytes in JSON.
var b64 = base64.RawURLEncoding

func decodeB64(s string) ([]byte, error) { return b64.DecodeString(strings.TrimRight(s, "=")) }

// newChallenge stores a fresh single-use challenge and returns it.
// accountID is "" for signing in (nobody known yet).
func newChallenge(ctx context.Context, pool *pgxpool.Pool, purpose, accountID string) ([]byte, error) {
	c := make([]byte, 32)
	rand.Read(c)
	_, err := pool.Exec(ctx, `
		INSERT INTO passkey_challenge (id, challenge, purpose, account_id, expires_at)
		VALUES ($1, $2, $3, NULLIF($4, '')::uuid, now() + $5::int * interval '1 second')`,
		newUUIDv7(), c, purpose, accountID, int(passkeyChallengeTTL.Seconds()))
	return c, err
}

// claimChallenge uses up a challenge: unused, unexpired, for this purpose and
// account. The conditional UPDATE lets only one request win, like a login
// token. It runs on its own (not in the caller's transaction), so a challenge
// is spent even when the answer then fails a check.
func claimChallenge(ctx context.Context, pool *pgxpool.Pool, challenge []byte, purpose, accountID string) (bool, error) {
	tag, err := pool.Exec(ctx, `
		UPDATE passkey_challenge SET used_at = now()
		WHERE challenge = $1 AND purpose = $2 AND used_at IS NULL AND expires_at > now()
		  AND account_id IS NOT DISTINCT FROM NULLIF($3, '')::uuid`,
		challenge, purpose, accountID)
	return tag.RowsAffected() == 1, err
}

// checkClientData reads the browser's clientDataJSON: what kind of request
// (wantType: "webauthn.create" or "webauthn.get"), from which page, for which
// challenge. The browser writes this itself, so a page on another site can't
// pretend to be ours. Returns the challenge, or why it's refused.
func checkClientData(raw []byte, wantType string, rp relyingParty) ([]byte, string) {
	var cd struct {
		Type        string `json:"type"`
		Challenge   string `json:"challenge"`
		Origin      string `json:"origin"`
		CrossOrigin bool   `json:"crossOrigin"`
	}
	if err := json.Unmarshal(raw, &cd); err != nil {
		return nil, "client data is not JSON"
	}
	switch {
	case cd.Type != wantType:
		return nil, "client data type is " + cd.Type
	case cd.Origin != rp.origin || cd.CrossOrigin:
		return nil, "made on another site: " + cd.Origin
	}
	challenge, err := decodeB64(cd.Challenge)
	if err != nil || len(challenge) != 32 {
		return nil, "no challenge of ours"
	}
	return challenge, ""
}

// authData is the authenticator data: what the device itself reports
// (WebAuthn spec, §6.1). Its layout, in bytes:
//
//	32  SHA-256 of the rpID the device made the key for
//	 1  flags (flagUserPresent, ...)
//	 4  signature counter, big-endian
//	-- only with flagAttestedData (registration):
//	16  AAGUID (the device model; zeros with attestation "none")
//	 2  credential ID length L, big-endian
//	 L  credential ID
//	 …  the public key in COSE form (we use the browser's DER copy instead)
type authData struct {
	rpIDHash     []byte
	flags        byte
	signCount    uint32
	credentialID []byte // registration only
}

func parseAuthData(b []byte) (authData, bool) {
	if len(b) < 37 {
		return authData{}, false
	}
	ad := authData{rpIDHash: b[:32], flags: b[32], signCount: binary.BigEndian.Uint32(b[33:37])}
	if ad.flags&flagAttestedData != 0 {
		if len(b) < 55 {
			return authData{}, false
		}
		n := int(binary.BigEndian.Uint16(b[53:55]))
		if len(b) < 55+n {
			return authData{}, false
		}
		ad.credentialID = b[55 : 55+n]
	}
	return ad, true
}

// checkAuthData: made for our site, with the person present and verified.
func checkAuthData(ad authData, rp relyingParty) string {
	want := sha256.Sum256([]byte(rp.id))
	switch {
	case !bytes.Equal(ad.rpIDHash, want[:]):
		return "the key is for another site"
	case ad.flags&flagUserPresent == 0:
		return "nobody was present"
	case ad.flags&flagUserVerified == 0:
		return "the person wasn't verified (no Face ID, fingerprint or PIN)"
	}
	return ""
}

// checkPublicKey: a key we can verify signatures with later, matching the
// algorithm the browser named.
func checkPublicKey(der []byte, alg int) string {
	key, err := x509.ParsePKIXPublicKey(der)
	if err != nil {
		return "the public key can't be read"
	}
	switch k := key.(type) {
	case *ecdsa.PublicKey:
		if alg != coseES256 || k.Curve != elliptic.P256() {
			return "an EC key that isn't ES256 on P-256"
		}
	case *rsa.PublicKey:
		if alg != coseRS256 || k.N.BitLen() < 2048 {
			return "an RSA key that isn't RS256 with at least 2048 bits"
		}
	default:
		return "a kind of key we don't accept"
	}
	return ""
}

// passkeyRegisterStart handles POST /me/passkeys/start (wrapped in
// requireSignIn): the options for navigator.credentials.create(), in
// WebAuthn's JSON form (bytes as base64url).
func passkeyRegisterStart(pool *pgxpool.Pool, rp relyingParty) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		me, _ := accountFrom(r)
		challenge, err := newChallenge(r.Context(), pool, "register", me.ID)
		if err != nil {
			logFrom(r).Error("passkey start: challenge failed", "err", err)
			writeError(w, r, http.StatusInternalServerError, "could not start adding a passkey")
			return
		}
		// The passkeys this account already has: the device then says "already
		// added" instead of making a second one for the same account.
		rows, _ := pool.Query(r.Context(), `
			SELECT credential_id FROM passkey WHERE account_id = $1 AND status = 'active'`, me.ID)
		ids, err := pgx.CollectRows(rows, pgx.RowTo[[]byte])
		if err != nil {
			logFrom(r).Error("passkey start: list failed", "err", err)
			writeError(w, r, http.StatusInternalServerError, "could not start adding a passkey")
			return
		}
		exclude := []map[string]string{}
		for _, id := range ids {
			exclude = append(exclude, map[string]string{"type": "public-key", "id": b64.EncodeToString(id)})
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"challenge": b64.EncodeToString(challenge),
			"rp":        map[string]string{"id": rp.id, "name": "Hablemos"},
			// user.id is what the device files the passkey under: the account
			// ID (not the email, which may change). name is what the phone
			// shows in its list ("Hablemos · ana@example.com").
			"user": map[string]string{"id": b64.EncodeToString([]byte(me.ID)), "name": me.Email, "displayName": me.DisplayName},
			"pubKeyCredParams": []map[string]any{
				{"type": "public-key", "alg": coseES256},
				{"type": "public-key", "alg": coseRS256},
			},
			"timeout":     passkeyChallengeTTL.Milliseconds(),
			"attestation": "none",
			// residentKey "required": the passkey lives on the device with the
			// account in it, so signing in needs no email typed ("discoverable").
			"authenticatorSelection": map[string]any{
				"residentKey": "required", "requireResidentKey": true, "userVerification": "required",
			},
			"excludeCredentials": exclude,
		})
	}
}

type passkeyDTO struct {
	ID         string     `json:"id"`
	Name       string     `json:"name"`
	CreatedAt  time.Time  `json:"created_at"`
	LastUsedAt *time.Time `json:"last_used_at"` // null until it's used to sign in
}

// passkeyRegisterFinish handles POST /me/passkeys (wrapped in requireSignIn):
// the device's answer to the challenge from /start, as the page copied it:
//
//	{"id", "client_data_json", "authenticator_data", "public_key" (base64url),
//	 "public_key_algorithm", "name"}
//
// Codes: challenge_invalid (unknown, used, expired or another account's: start
// again), passkey_invalid (fails a check; the reason is logged), passkey_exists,
// too_many_passkeys.
func passkeyRegisterFinish(pool *pgxpool.Pool, rp relyingParty) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		me, _ := accountFrom(r)
		var body struct {
			ID                 string `json:"id"`
			ClientDataJSON     string `json:"client_data_json"`
			AuthenticatorData  string `json:"authenticator_data"`
			PublicKey          string `json:"public_key"`
			PublicKeyAlgorithm int    `json:"public_key_algorithm"`
			Name               string `json:"name"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&body); err != nil {
			writeError(w, r, http.StatusBadRequest, "body must be JSON with the passkey the device made")
			return
		}
		refuse := func(reason string) {
			logFrom(r).Info("passkey refused", "reason", reason, "account_id", me.ID)
			writeErrorCode(w, r, http.StatusBadRequest, "passkey_invalid", "this passkey can't be added: "+reason)
		}
		credID, err1 := decodeB64(body.ID)
		clientData, err2 := decodeB64(body.ClientDataJSON)
		rawAuth, err3 := decodeB64(body.AuthenticatorData)
		publicKey, err4 := decodeB64(body.PublicKey)
		if err := errors.Join(err1, err2, err3, err4); err != nil || len(credID) == 0 || len(credID) > 1023 {
			refuse("fields aren't base64url")
			return
		}
		challenge, reason := checkClientData(clientData, "webauthn.create", rp)
		if reason != "" {
			refuse(reason)
			return
		}
		switch ok, err := claimChallenge(r.Context(), pool, challenge, "register", me.ID); {
		case err != nil:
			logFrom(r).Error("passkey finish: claim challenge failed", "err", err)
			writeError(w, r, http.StatusInternalServerError, "could not add the passkey")
			return
		case !ok:
			writeErrorCode(w, r, http.StatusBadRequest, "challenge_invalid", "this attempt has expired; please try again")
			return
		}
		ad, ok := parseAuthData(rawAuth)
		switch {
		case !ok:
			reason = "authenticator data is cut short"
		case ad.flags&flagAttestedData == 0:
			reason = "no new credential in the authenticator data"
		case !bytes.Equal(ad.credentialID, credID):
			reason = "the credential ID doesn't match the authenticator data"
		default:
			reason = checkAuthData(ad, rp)
		}
		if reason == "" {
			reason = checkPublicKey(publicKey, body.PublicKeyAlgorithm)
		}
		if reason != "" {
			refuse(reason)
			return
		}
		name := strings.TrimSpace(body.Name)
		if name == "" {
			name = "Passkey"
		}
		if utf8.RuneCountInString(name) > 60 {
			name = string([]rune(name)[:60])
		}

		var p passkeyDTO
		err := pgx.BeginFunc(r.Context(), pool, func(tx pgx.Tx) error {
			// The account lock makes the count and the insert one step, so two
			// tabs can't both add the 10th passkey (as with learners).
			if _, err := tx.Exec(r.Context(), `SELECT 1 FROM account WHERE id = $1 FOR UPDATE`, me.ID); err != nil {
				return err
			}
			var n int
			if err := tx.QueryRow(r.Context(), `
				SELECT count(*) FROM passkey WHERE account_id = $1 AND status = 'active'`, me.ID).Scan(&n); err != nil {
				return err
			}
			if n >= maxPasskeys {
				return notBookable("too_many_passkeys", "an account can have at most 10 passkeys; remove one first")
			}
			p.ID, p.Name = newUUIDv7(), name
			return tx.QueryRow(r.Context(), `
				INSERT INTO passkey (id, account_id, credential_id, public_key, algorithm, sign_count, name)
				VALUES ($1, $2, $3, $4, $5, $6, $7) RETURNING created_at`,
				p.ID, me.ID, credID, publicKey, body.PublicKeyAlgorithm, int64(ad.signCount), name).Scan(&p.CreatedAt)
		})
		var be *bookingError
		var pgErr *pgconn.PgError
		switch {
		case errors.As(err, &pgErr) && pgErr.Code == "23505": // passkey_credential_id_key
			writeErrorCode(w, r, http.StatusConflict, "passkey_exists", "this passkey has already been added")
		case errors.As(err, &be):
			writeErrorCode(w, r, be.status, be.code, be.msg)
		case err != nil:
			logFrom(r).Error("passkey finish: save failed", "err", err)
			writeError(w, r, http.StatusInternalServerError, "could not add the passkey")
		default:
			logFrom(r).Info("passkey added", "account_id", me.ID, "passkey_id", p.ID, "algorithm", body.PublicKeyAlgorithm)
			writeJSON(w, http.StatusCreated, map[string]any{"passkey": p})
		}
	}
}

// listPasskeys handles GET /me/passkeys (wrapped in requireSignIn): my active
// passkeys, newest first, for the list in My Account.
func listPasskeys(pool *pgxpool.Pool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		me, _ := accountFrom(r)
		rows, _ := pool.Query(r.Context(), `
			SELECT id::text, name, created_at, last_used_at FROM passkey
			WHERE account_id = $1 AND status = 'active' ORDER BY created_at DESC`, me.ID)
		list, err := pgx.CollectRows(rows, pgx.RowToStructByPos[passkeyDTO])
		if err != nil {
			logFrom(r).Error("list passkeys failed", "err", err)
			writeError(w, r, http.StatusInternalServerError, "could not load your passkeys")
			return
		}
		writeJSON(w, http.StatusOK, list)
	}
}

// removePasskey handles POST /me/passkeys/{id}/remove (wrapped in
// requireSignIn): status 'removed' (never deleted); it can't sign in any more.
// The phone still holds its half; the person can delete it there too. Mine
// only, else 404.
func removePasskey(pool *pgxpool.Pool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		me, _ := accountFrom(r)
		id := r.PathValue("id")
		if !isUUID(id) {
			writeError(w, r, http.StatusBadRequest, "the passkey ID in the path is not valid")
			return
		}
		tag, err := pool.Exec(r.Context(), `
			UPDATE passkey SET status = 'removed' WHERE id = $1 AND account_id = $2 AND status = 'active'`, id, me.ID)
		switch {
		case err != nil:
			logFrom(r).Error("remove passkey failed", "err", err)
			writeError(w, r, http.StatusInternalServerError, "could not remove the passkey")
		case tag.RowsAffected() == 0:
			writeErrorCode(w, r, http.StatusNotFound, "passkey_not_found", "no such passkey on your account")
		default:
			logFrom(r).Info("passkey removed", "account_id", me.ID, "passkey_id", id)
			writeJSON(w, http.StatusOK, map[string]string{"id": id, "status": "removed"})
		}
	}
}
