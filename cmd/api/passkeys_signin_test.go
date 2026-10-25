package main

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// sign is what the device does after Face ID: sign its authenticator data
// followed by the SHA-256 of the client data.
func (a *fakeAuthenticator) sign(authData, clientData []byte) []byte {
	cdHash := sha256.Sum256(clientData)
	digest := sha256.Sum256(append(append([]byte{}, authData...), cdHash[:]...))
	sig, err := a.key.Sign(rand.Reader, digest[:], crypto.SHA256) // ASN.1 for ECDSA, PKCS #1 v1.5 for RSA
	if err != nil {
		panic(err)
	}
	return sig
}

// assertion is the body the page sends to POST /auth/passkey: a signed
// answer to challenge, with the device's counter at count.
func (a *fakeAuthenticator) assertion(challenge []byte, count uint32, accountID string) map[string]any {
	ad := a.authData(testRP.id, flagUserPresent|flagUserVerified, count, false)
	cd := clientDataJSON("webauthn.get", challenge, testRP.origin)
	return map[string]any{
		"id":                 b64.EncodeToString(a.credID),
		"client_data_json":   b64.EncodeToString(cd),
		"authenticator_data": b64.EncodeToString(ad),
		"signature":          b64.EncodeToString(a.sign(ad, cd)),
		"user_handle":        b64.EncodeToString([]byte(accountID)),
	}
}

// registerFake adds a's passkey to s's account, as the page will.
func registerFake(t *testing.T, pool *pgxpool.Pool, s student, a *fakeAuthenticator) {
	t.Helper()
	c, _ := startRegistration(t, pool, s)
	if rec := finishRegistration(t, s, a.registration(c)); rec.Code != 201 {
		t.Fatalf("register: %d %s", rec.Code, rec.Body)
	}
}

func startSignin(t *testing.T, pool *pgxpool.Pool) []byte {
	t.Helper()
	rec := postLogin(t, passkeySigninStart(pool, testRP), "")
	var opts struct{ Challenge, RpID string }
	json.Unmarshal(rec.Body.Bytes(), &opts)
	c, err := decodeB64(opts.Challenge)
	if rec.Code != 200 || err != nil || len(c) != 32 || opts.RpID != "site.test" {
		t.Fatalf("start: %d %s", rec.Code, rec.Body)
	}
	return c
}

func postSignin(t *testing.T, pool *pgxpool.Pool, body map[string]any) *httptest.ResponseRecorder {
	b, _ := json.Marshal(body)
	return postLogin(t, passkeySignin(pool, testRP), string(b))
}

// Each check in POST /auth/passkey: a real signed answer signs in; the same
// answer with one thing wrong doesn't.
func TestPasskeySigninChecks(t *testing.T) {
	pool := testPool(t)
	s := newStudent(t, pool, 0, false)
	a := newFakeAuthenticator(t)
	registerFake(t, pool, s, a)
	other := newStudent(t, pool, 0, false)

	resign := func(b map[string]any, ad, cd []byte) {
		b["authenticator_data"], b["client_data_json"] = b64.EncodeToString(ad), b64.EncodeToString(cd)
		b["signature"] = b64.EncodeToString(a.sign(ad, cd))
	}
	tests := []struct {
		name  string
		spoil func(c []byte, b map[string]any)
		code  string // "" = signed in
	}{
		{"a real answer", func([]byte, map[string]any) {}, ""},
		{"without a user handle (optional)", func(c []byte, b map[string]any) { delete(b, "user_handle") }, ""},
		{"a registration, not a sign-in", func(c []byte, b map[string]any) {
			resign(b, a.authData(testRP.id, flagUserPresent|flagUserVerified, 0, false), clientDataJSON("webauthn.create", c, testRP.origin))
		}, "passkey_invalid"},
		{"made on another site's page", func(c []byte, b map[string]any) {
			resign(b, a.authData(testRP.id, flagUserPresent|flagUserVerified, 0, false), clientDataJSON("webauthn.get", c, "https://evil.test"))
		}, "passkey_invalid"},
		{"signed for another site", func(c []byte, b map[string]any) {
			resign(b, a.authData("evil.test", flagUserPresent|flagUserVerified, 0, false), clientDataJSON("webauthn.get", c, testRP.origin))
		}, "passkey_invalid"},
		{"no Face ID (present, not verified)", func(c []byte, b map[string]any) {
			resign(b, a.authData(testRP.id, flagUserPresent, 0, false), clientDataJSON("webauthn.get", c, testRP.origin))
		}, "passkey_invalid"},
		{"signed by another key", func(c []byte, b map[string]any) {
			ad, cd := a.authData(testRP.id, flagUserPresent|flagUserVerified, 0, false), clientDataJSON("webauthn.get", c, testRP.origin)
			b["signature"] = b64.EncodeToString(newFakeAuthenticator(t).sign(ad, cd))
		}, "passkey_invalid"},
		{"data changed after signing", func(c []byte, b map[string]any) {
			b["authenticator_data"] = b64.EncodeToString(a.authData(testRP.id, flagUserPresent|flagUserVerified, 9, false))
		}, "passkey_invalid"},
		{"names another account", func(c []byte, b map[string]any) {
			b["user_handle"] = b64.EncodeToString([]byte(other.accountID))
		}, "passkey_invalid"},
		{"a passkey that isn't ours", func(c []byte, b map[string]any) { b["id"] = b64.EncodeToString([]byte("unknown")) }, "passkey_unknown"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := startSignin(t, pool)
			body := a.assertion(c, 0, s.accountID)
			tt.spoil(c, body)
			rec := postSignin(t, pool, body)
			if tt.code != "" {
				if rec.Code != 400 || errorCode(t, rec) != tt.code {
					t.Errorf("%d %s, want 400 %s", rec.Code, rec.Body, tt.code)
				}
				return
			}
			var got struct{ Account account }
			json.Unmarshal(rec.Body.Bytes(), &got)
			if rec.Code != 200 || got.Account.ID != s.accountID || !strings.Contains(rec.Header().Get("Set-Cookie"), sessionCookie+"=") {
				t.Errorf("%d %s", rec.Code, rec.Body)
			}
		})
	}
	if n := count(t, pool, `SELECT count(*) FROM passkey WHERE account_id = $1 AND last_used_at IS NOT NULL`, s.accountID); n != 1 {
		t.Errorf("last_used_at not set")
	}
}

// An RSA passkey; the challenge (once, sign-in only); a removed passkey; a
// disabled account.
func TestPasskeySigninMore(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	s := newStudent(t, pool, 0, false)
	a := newFakeAuthenticator(t)
	registerFake(t, pool, s, a)

	t.Run("an RSA passkey", func(t *testing.T) {
		r := newRSAFakeAuthenticator(t)
		registerFake(t, pool, s, r)
		if rec := postSignin(t, pool, r.assertion(startSignin(t, pool), 0, s.accountID)); rec.Code != 200 {
			t.Errorf("%d %s", rec.Code, rec.Body)
		}
	})
	t.Run("the same challenge twice", func(t *testing.T) {
		c := startSignin(t, pool)
		postSignin(t, pool, a.assertion(c, 0, s.accountID))
		if rec := postSignin(t, pool, a.assertion(c, 0, s.accountID)); errorCode(t, rec) != "challenge_invalid" {
			t.Errorf("%d %s", rec.Code, rec.Body)
		}
	})
	t.Run("a challenge for adding a passkey", func(t *testing.T) {
		c, _ := startRegistration(t, pool, s)
		if rec := postSignin(t, pool, a.assertion(c, 0, s.accountID)); errorCode(t, rec) != "challenge_invalid" {
			t.Errorf("%d %s", rec.Code, rec.Body)
		}
	})
	t.Run("a removed passkey", func(t *testing.T) {
		gone := newFakeAuthenticator(t)
		registerFake(t, pool, s, gone)
		pool.Exec(ctx, `UPDATE passkey SET status = 'removed' WHERE credential_id = $1`, gone.credID)
		if rec := postSignin(t, pool, gone.assertion(startSignin(t, pool), 0, s.accountID)); errorCode(t, rec) != "passkey_unknown" {
			t.Errorf("%d %s", rec.Code, rec.Body)
		}
	})
	t.Run("a disabled account", func(t *testing.T) {
		d := newStudent(t, pool, 0, false)
		da := newFakeAuthenticator(t)
		registerFake(t, pool, d, da)
		pool.Exec(ctx, `UPDATE account SET status = 'disabled' WHERE id = $1`, d.accountID)
		if rec := postSignin(t, pool, da.assertion(startSignin(t, pool), 0, d.accountID)); rec.Code != 403 {
			t.Errorf("%d %s", rec.Code, rec.Body)
		}
	})
}

// A device that keeps a counter: it must go up every time, or the key may
// have been copied. With the passkey row held locked (a gate), two sign-ins
// sending the same next value wait; released, only one gets in. Without the
// FOR UPDATE both would read the old counter and both pass.
func TestPasskeySigninCounter(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	s := newStudent(t, pool, 0, false)
	a := newFakeAuthenticator(t)
	registerFake(t, pool, s, a)

	if rec := postSignin(t, pool, a.assertion(startSignin(t, pool), 5, s.accountID)); rec.Code != 200 {
		t.Fatalf("5: %d %s", rec.Code, rec.Body)
	}
	if rec := postSignin(t, pool, a.assertion(startSignin(t, pool), 5, s.accountID)); errorCode(t, rec) != "passkey_invalid" {
		t.Errorf("5 again: %d %s", rec.Code, rec.Body)
	}

	gate, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer gate.Rollback(ctx)
	if _, err := gate.Exec(ctx, `SELECT 1 FROM passkey WHERE credential_id = $1 FOR UPDATE`, a.credID); err != nil {
		t.Fatal(err)
	}
	bodies := []map[string]any{a.assertion(startSignin(t, pool), 6, s.accountID), a.assertion(startSignin(t, pool), 6, s.accountID)}
	codes := make(chan int, 2)
	for _, b := range bodies {
		go func() { codes <- postSignin(t, pool, b).Code }()
	}
	waitForLockWaiters(t, pool, 2)
	gate.Rollback(ctx)
	got := map[int]int{}
	for range 2 {
		got[<-codes]++
	}
	if got[200] != 1 || got[400] != 1 {
		t.Errorf("answers %v, want one 200 and one 400", got)
	}
}

// The worker deletes challenges a day past their expiry, and keeps newer ones.
func TestPurgePasskeyChallenges(t *testing.T) {
	pool := migratedSchemaPool(t, 13)
	ctx := context.Background()
	add := func(expiredAgo string) {
		t.Helper()
		if _, err := pool.Exec(ctx, `
			INSERT INTO passkey_challenge (id, challenge, purpose, created_at, expires_at)
			VALUES ($1, $2, 'signin', now() - $3::interval - interval '5 min', now() - $3::interval)`,
			newUUIDv7(), hashSecret(newSecret()), expiredAgo); err != nil {
			t.Fatal(err)
		}
	}
	add("2 days") // gone
	add("1 hour") // kept
	if n, err := purgePasskeyChallenges(ctx, pool); err != nil || n != 1 {
		t.Errorf("purged %d (err %v), want 1", n, err)
	}
}
