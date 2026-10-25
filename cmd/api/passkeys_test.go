package main

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/binary"
	"encoding/json"
	"net/http/httptest"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// The site the tests pretend to be.
var testRP = relyingPartyFor("https://site.test")

// fakeAuthenticator stands in for a phone: it holds a key pair and answers
// the way a real device and browser do (the same bytes, the same JSON).
// crypto.Signer is what both kinds of private key have in common.
type fakeAuthenticator struct {
	key    crypto.Signer
	alg    int // coseES256 or coseRS256
	credID []byte
}

func newFakeAuthenticator(t *testing.T) *fakeAuthenticator {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	id := make([]byte, 16)
	rand.Read(id)
	return &fakeAuthenticator{key: key, alg: coseES256, credID: id}
}

// newRSAFakeAuthenticator: like some Windows Hello keys.
func newRSAFakeAuthenticator(t *testing.T) *fakeAuthenticator {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	id := make([]byte, 16)
	rand.Read(id)
	return &fakeAuthenticator{key: key, alg: coseRS256, credID: id}
}

// authData builds the authenticator data (layout in passkeys.go) for rpID.
// With a credential (registration), the credential ID follows; the COSE key
// after it is left out (we read the browser's DER copy instead) except for a
// few stand-in bytes.
func (a *fakeAuthenticator) authData(rpID string, flags byte, count uint32, withCred bool) []byte {
	h := sha256.Sum256([]byte(rpID))
	b := append(h[:], flags)
	b = binary.BigEndian.AppendUint32(b, count)
	if withCred {
		b = append(b, make([]byte, 16)...) // AAGUID: zeros, as with attestation "none"
		b = binary.BigEndian.AppendUint16(b, uint16(len(a.credID)))
		b = append(b, a.credID...)
		b = append(b, 0xa5, 0x01, 0x02) // the start of a COSE key
	}
	return b
}

func clientDataJSON(typ string, challenge []byte, origin string) []byte {
	b, _ := json.Marshal(map[string]any{"type": typ, "challenge": b64.EncodeToString(challenge), "origin": origin, "crossOrigin": false})
	return b
}

// registration is the body the page sends to POST /me/passkeys, as a map so a
// test can spoil one field.
func (a *fakeAuthenticator) registration(challenge []byte) map[string]any {
	der, _ := x509.MarshalPKIXPublicKey(a.key.Public())
	return map[string]any{
		"id":                   b64.EncodeToString(a.credID),
		"client_data_json":     b64.EncodeToString(clientDataJSON("webauthn.create", challenge, testRP.origin)),
		"authenticator_data":   b64.EncodeToString(a.authData(testRP.id, flagUserPresent|flagUserVerified|flagAttestedData, 0, true)),
		"public_key":           b64.EncodeToString(der),
		"public_key_algorithm": a.alg,
		"name":                 "iPhone · Safari",
	}
}

// startRegistration calls POST /me/passkeys/start and returns the challenge
// and the IDs it says to exclude.
func startRegistration(t *testing.T, pool *pgxpool.Pool, s student) ([]byte, []string) {
	t.Helper()
	rec := me(s, requireSignIn(pool, passkeyRegisterStart(pool, testRP)), "POST", "", "")
	if rec.Code != 200 {
		t.Fatalf("start: %d %s", rec.Code, rec.Body)
	}
	var opts struct {
		Challenge          string
		RP                 struct{ ID string }
		ExcludeCredentials []struct{ ID string }
	}
	json.Unmarshal(rec.Body.Bytes(), &opts)
	if opts.RP.ID != "site.test" {
		t.Errorf("rp.id = %q", opts.RP.ID)
	}
	c, err := decodeB64(opts.Challenge)
	if err != nil || len(c) != 32 {
		t.Fatalf("challenge %q", opts.Challenge)
	}
	var ex []string
	for _, e := range opts.ExcludeCredentials {
		ex = append(ex, e.ID)
	}
	return c, ex
}

func finishRegistration(t *testing.T, s student, body map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	b, _ := json.Marshal(body)
	pool := testPool(t)
	return me(s, requireSignIn(pool, passkeyRegisterFinish(pool, testRP)), "POST", string(b), "")
}

// Each check in POST /me/passkeys: a real answer is accepted; the same answer
// with one thing wrong is refused.
func TestPasskeyRegisterChecks(t *testing.T) {
	pool := testPool(t)
	s := newStudent(t, pool, 0, false)
	p384, _ := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	rsa1024, _ := rsa.GenerateKey(rand.Reader, 1024)
	der := func(k any) string { b, _ := x509.MarshalPKIXPublicKey(k); return b64.EncodeToString(b) }

	tests := []struct {
		name  string
		spoil func(a *fakeAuthenticator, challenge []byte, body map[string]any)
		code  int
	}{
		{"a real answer", func(*fakeAuthenticator, []byte, map[string]any) {}, 201},
		{"a sign-in answer, not a new key", func(a *fakeAuthenticator, c []byte, b map[string]any) {
			b["client_data_json"] = b64.EncodeToString(clientDataJSON("webauthn.get", c, testRP.origin))
		}, 400},
		{"made on another site's page", func(a *fakeAuthenticator, c []byte, b map[string]any) {
			b["client_data_json"] = b64.EncodeToString(clientDataJSON("webauthn.create", c, "https://evil.test"))
		}, 400},
		{"a key for another site", func(a *fakeAuthenticator, c []byte, b map[string]any) {
			b["authenticator_data"] = b64.EncodeToString(a.authData("evil.test", flagUserPresent|flagUserVerified|flagAttestedData, 0, true))
		}, 400},
		{"no Face ID (present, not verified)", func(a *fakeAuthenticator, c []byte, b map[string]any) {
			b["authenticator_data"] = b64.EncodeToString(a.authData(testRP.id, flagUserPresent|flagAttestedData, 0, true))
		}, 400},
		{"nobody present", func(a *fakeAuthenticator, c []byte, b map[string]any) {
			b["authenticator_data"] = b64.EncodeToString(a.authData(testRP.id, flagUserVerified|flagAttestedData, 0, true))
		}, 400},
		{"no new credential in the data", func(a *fakeAuthenticator, c []byte, b map[string]any) {
			b["authenticator_data"] = b64.EncodeToString(a.authData(testRP.id, flagUserPresent|flagUserVerified, 0, false))
		}, 400},
		{"another credential ID", func(a *fakeAuthenticator, c []byte, b map[string]any) {
			b["id"] = b64.EncodeToString([]byte("someone else's"))
		}, 400},
		{"authenticator data cut short", func(a *fakeAuthenticator, c []byte, b map[string]any) {
			b["authenticator_data"] = b64.EncodeToString(a.authData(testRP.id, flagUserPresent|flagUserVerified|flagAttestedData, 0, true)[:50])
		}, 400},
		{"an EC key that isn't P-256", func(a *fakeAuthenticator, c []byte, b map[string]any) { b["public_key"] = der(&p384.PublicKey) }, 400},
		{"EC key, RSA algorithm named", func(a *fakeAuthenticator, c []byte, b map[string]any) { b["public_key_algorithm"] = coseRS256 }, 400},
		{"a weak RSA key", func(a *fakeAuthenticator, c []byte, b map[string]any) {
			b["public_key"], b["public_key_algorithm"] = der(&rsa1024.PublicKey), coseRS256
		}, 400},
		{"not a key at all", func(a *fakeAuthenticator, c []byte, b map[string]any) {
			b["public_key"] = b64.EncodeToString([]byte("hello"))
		}, 400},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := newFakeAuthenticator(t)
			c, _ := startRegistration(t, pool, s)
			body := a.registration(c)
			tt.spoil(a, c, body)
			rec := finishRegistration(t, s, body)
			if rec.Code != tt.code {
				t.Fatalf("%d %s, want %d", rec.Code, rec.Body, tt.code)
			}
			if tt.code == 400 && errorCode(t, rec) != "passkey_invalid" {
				t.Errorf("code %q", errorCode(t, rec))
			}
		})
	}
	if n := count(t, pool, `SELECT count(*) FROM passkey WHERE account_id = $1`, s.accountID); n != 1 {
		t.Errorf("%d passkeys stored, want 1 (only the real answer)", n)
	}
}

// The challenge: works once, only for its own account, only while fresh; a
// passkey can't be added twice; start lists the ones to exclude; at most 10.
func TestPasskeyRegisterChallengeAndLimits(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	s := newStudent(t, pool, 0, false)
	a := newFakeAuthenticator(t)

	c, _ := startRegistration(t, pool, s)
	if rec := finishRegistration(t, s, a.registration(c)); rec.Code != 201 {
		t.Fatalf("first: %d %s", rec.Code, rec.Body)
	}
	t.Run("the same challenge again", func(t *testing.T) {
		if rec := finishRegistration(t, s, newFakeAuthenticator(t).registration(c)); errorCode(t, rec) != "challenge_invalid" {
			t.Errorf("%d %s", rec.Code, rec.Body)
		}
	})
	t.Run("another account's challenge", func(t *testing.T) {
		other := newStudent(t, pool, 0, false)
		c, _ := startRegistration(t, pool, other)
		if rec := finishRegistration(t, s, newFakeAuthenticator(t).registration(c)); errorCode(t, rec) != "challenge_invalid" {
			t.Errorf("%d %s", rec.Code, rec.Body)
		}
	})
	t.Run("an expired challenge", func(t *testing.T) {
		c, _ := startRegistration(t, pool, s)
		pool.Exec(ctx, `UPDATE passkey_challenge SET created_at = now() - interval '10 min', expires_at = now() - interval '1 min' WHERE challenge = $1`, c)
		if rec := finishRegistration(t, s, newFakeAuthenticator(t).registration(c)); errorCode(t, rec) != "challenge_invalid" {
			t.Errorf("%d %s", rec.Code, rec.Body)
		}
	})
	t.Run("the same passkey twice", func(t *testing.T) {
		c, exclude := startRegistration(t, pool, s)
		if len(exclude) != 1 || exclude[0] != b64.EncodeToString(a.credID) {
			t.Errorf("excludeCredentials = %v", exclude)
		}
		if rec := finishRegistration(t, s, a.registration(c)); rec.Code != 409 || errorCode(t, rec) != "passkey_exists" {
			t.Errorf("%d %s", rec.Code, rec.Body)
		}
	})
	t.Run("at most 10", func(t *testing.T) {
		for range maxPasskeys - 1 {
			c, _ := startRegistration(t, pool, s)
			if rec := finishRegistration(t, s, newFakeAuthenticator(t).registration(c)); rec.Code != 201 {
				t.Fatalf("%d %s", rec.Code, rec.Body)
			}
		}
		c, _ := startRegistration(t, pool, s)
		if rec := finishRegistration(t, s, newFakeAuthenticator(t).registration(c)); rec.Code != 409 || errorCode(t, rec) != "too_many_passkeys" {
			t.Errorf("11th: %d %s", rec.Code, rec.Body)
		}
	})
}
