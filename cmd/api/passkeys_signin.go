package main

// Passkeys (WebAuthn): signing in with one. Adding one is in passkeys.go.
//
//	POST /auth/passkey/start → options for navigator.credentials.get(), with a
//	                           fresh challenge (public: nobody is known yet)
//	POST /auth/passkey       → the device's signature over that challenge;
//	                           checked with the stored public key → web session
//
// No email is typed: the passkey was made "discoverable", so the phone itself
// offers the accounts it holds for this site, and its answer names the
// credential, which is how we find the account.

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// passkeySigninStart handles POST /auth/passkey/start: the options for
// navigator.credentials.get(). allowCredentials is empty: any passkey of
// ours on the device may answer (the phone asks which, if there are several).
func passkeySigninStart(pool *pgxpool.Pool, rp relyingParty) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		challenge, err := newChallenge(r.Context(), pool, "signin", "")
		if err != nil {
			logFrom(r).Error("passkey sign-in start: challenge failed", "err", err)
			writeError(w, r, http.StatusInternalServerError, "could not start signing in")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"challenge":        b64.EncodeToString(challenge),
			"rpId":             rp.id,
			"timeout":          passkeyChallengeTTL.Milliseconds(),
			"userVerification": "required",
			"allowCredentials": []any{},
		})
	}
}

// verifySignature checks the device's signature. What the device signs is its
// authenticator data followed by the SHA-256 of the client data, so the
// signature covers our challenge, our origin and the flags all at once.
func verifySignature(publicKey []byte, alg int, authData, clientData, sig []byte) bool {
	key, err := x509.ParsePKIXPublicKey(publicKey)
	if err != nil {
		return false
	}
	cdHash := sha256.Sum256(clientData)
	digest := sha256.Sum256(append(append([]byte{}, authData...), cdHash[:]...))
	switch k := key.(type) {
	case *ecdsa.PublicKey:
		return alg == coseES256 && ecdsa.VerifyASN1(k, digest[:], sig)
	case *rsa.PublicKey:
		return alg == coseRS256 && rsa.VerifyPKCS1v15(k, crypto.SHA256, digest[:], sig) == nil
	}
	return false
}

// passkeySignin handles POST /auth/passkey:
//
//	{"id", "client_data_json", "authenticator_data", "signature", "user_handle"
//	 (base64url), "timezone"}
//
// Codes: challenge_invalid (start again), passkey_unknown (not ours, or
// removed: sign in by email instead), passkey_invalid (fails a check; the
// reason is logged), 403 for a disabled account.
func passkeySignin(pool *pgxpool.Pool, rp relyingParty) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			ID                string `json:"id"`
			ClientDataJSON    string `json:"client_data_json"`
			AuthenticatorData string `json:"authenticator_data"`
			Signature         string `json:"signature"`
			UserHandle        string `json:"user_handle"`
			Timezone          string `json:"timezone"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&body); err != nil {
			writeError(w, r, http.StatusBadRequest, "body must be JSON with the device's answer")
			return
		}
		refuse := func(reason string) {
			logFrom(r).Info("passkey sign-in refused", "reason", reason)
			writeErrorCode(w, r, http.StatusBadRequest, "passkey_invalid", "this passkey can't sign you in: "+reason)
		}
		credID, err1 := decodeB64(body.ID)
		clientData, err2 := decodeB64(body.ClientDataJSON)
		rawAuth, err3 := decodeB64(body.AuthenticatorData)
		sig, err4 := decodeB64(body.Signature)
		userHandle, err5 := decodeB64(body.UserHandle)
		if err := errors.Join(err1, err2, err3, err4, err5); err != nil || len(credID) == 0 {
			refuse("fields aren't base64url")
			return
		}
		challenge, reason := checkClientData(clientData, "webauthn.get", rp)
		if reason != "" {
			refuse(reason)
			return
		}
		switch ok, err := claimChallenge(r.Context(), pool, challenge, "signin", ""); {
		case err != nil:
			logFrom(r).Error("passkey sign-in: claim challenge failed", "err", err)
			writeError(w, r, http.StatusInternalServerError, "could not sign you in")
			return
		case !ok:
			writeErrorCode(w, r, http.StatusBadRequest, "challenge_invalid", "this attempt has expired; please try again")
			return
		}
		ad, ok := parseAuthData(rawAuth)
		if !ok {
			refuse("authenticator data is cut short")
			return
		}
		if reason := checkAuthData(ad, rp); reason != "" {
			refuse(reason)
			return
		}

		var acct account
		var secret string
		err := pgx.BeginFunc(r.Context(), pool, func(tx pgx.Tx) error {
			// The passkey, locked: two sign-ins with the same counter value
			// then can't both pass the counter check below.
			var passkeyID, accountID, email string
			var publicKey []byte
			var alg int
			var stored int64
			err := tx.QueryRow(r.Context(), `
				SELECT p.id::text, p.account_id::text, a.email, p.public_key, p.algorithm, p.sign_count
				FROM passkey p JOIN account a ON a.id = p.account_id
				WHERE p.credential_id = $1 AND p.status = 'active'
				FOR UPDATE OF p`, credID).Scan(&passkeyID, &accountID, &email, &publicKey, &alg, &stored)
			if errors.Is(err, pgx.ErrNoRows) {
				return errPasskeyUnknown
			}
			if err != nil {
				return err
			}
			// user_handle is the user.id we gave at registration (the account
			// ID). Optional in the spec; when present, it must be this account.
			if len(userHandle) > 0 && string(userHandle) != accountID {
				return passkeyRefusal("the passkey names another account")
			}
			if !verifySignature(publicKey, alg, rawAuth, clientData, sig) {
				return passkeyRefusal("the signature doesn't match the passkey")
			}
			// The counter only goes up on devices that keep one (synced
			// passkeys always send 0). Not going up means the key may have
			// been copied off the device: refuse, and log it.
			if (stored > 0 || ad.signCount > 0) && int64(ad.signCount) <= stored {
				logFrom(r).Warn("passkey counter did not go up: possibly a cloned key",
					"passkey_id", passkeyID, "stored", stored, "got", ad.signCount)
				return passkeyRefusal("the passkey's counter didn't go up")
			}
			if _, err := tx.Exec(r.Context(), `
				UPDATE passkey SET sign_count = $2, last_used_at = now() WHERE id = $1`,
				passkeyID, int64(ad.signCount)); err != nil {
				return err
			}
			acct, secret, err = signInEmail(r.Context(), tx, email, body.Timezone)
			return err
		})
		var refusal passkeyRefusal
		switch {
		case errors.As(err, &refusal):
			refuse(string(refusal))
		case errors.Is(err, errPasskeyUnknown):
			writeErrorCode(w, r, http.StatusBadRequest, "passkey_unknown", "this passkey isn't registered here; sign in with your email")
		case errors.Is(err, errAccountDisabled):
			writeErrorCode(w, r, http.StatusForbidden, "account_disabled", "this account is disabled")
		case err != nil:
			logFrom(r).Error("passkey sign-in failed", "err", err)
			writeError(w, r, http.StatusInternalServerError, "could not sign you in")
		default:
			logFrom(r).Info("signed in with a passkey", "account_id", acct.ID)
			setSessionCookie(w, secret)
			writeJSON(w, http.StatusOK, map[string]any{"account": acct})
		}
	}
}

var errPasskeyUnknown = errors.New("passkey unknown or removed")

// purgePasskeyChallenges (worker) deletes challenges a day past their expiry:
// security records, not business records, and anyone can make one (POST
// /auth/passkey/start is public), so they mustn't pile up.
func purgePasskeyChallenges(ctx context.Context, pool *pgxpool.Pool) (int64, error) {
	tag, err := pool.Exec(ctx, `DELETE FROM passkey_challenge WHERE expires_at < now() - interval '1 day'`)
	return tag.RowsAffected(), err
}

// passkeyRefusal is a failed check inside the transaction, carrying the
// reason (an error type, so errors.As finds it after pgx.BeginFunc).
type passkeyRefusal string

func (p passkeyRefusal) Error() string { return string(p) }
