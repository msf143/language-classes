package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestNormalizeEmail(t *testing.T) {
	tests := []struct {
		in, want string
		ok       bool
	}{
		{"ana@example.com", "ana@example.com", true},
		{"  Ana@Example.COM ", "ana@example.com", true},
		{"", "", false},
		{"not-an-email", "", false},
		{"ana@localhost", "", false}, // no dot in the domain
		{"Ana <ana@example.com>", "", false},
		{"ana@example.com, bob@example.com", "", false},
		{strings.Repeat("a", 250) + "@example.com", "", false},
	}
	for _, tt := range tests {
		got, ok := normalizeEmail(tt.in)
		if got != tt.want || ok != tt.ok {
			t.Errorf("normalizeEmail(%q) = %q, %v; want %q, %v", tt.in, got, ok, tt.want, tt.ok)
		}
	}
}

// captureSender records the links that would have been emailed.
type captureSender struct{ links []string }

func (c *captureSender) send(r *http.Request, email, link string) error {
	c.links = append(c.links, link)
	return nil
}

// postLogin calls the handler directly with a JSON body and returns the response.
func postLogin(t *testing.T, h http.HandlerFunc, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("POST", "/auth/login", strings.NewReader(body))
	rec := httptest.NewRecorder()
	h(rec, req)
	return rec
}

// testEmail is unique per call, so tests never collide with each other or seed data.
func testEmail() string { return "test-" + newUUIDv7() + "@example.com" }

func TestRequestLoginLink(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()

	t.Run("bad input is rejected", func(t *testing.T) {
		var c captureSender
		h := requestLoginLink(pool, c.send, "https://site.test")
		for _, body := range []string{`not json`, `{"email": "nope"}`, `{}`} {
			if rec := postLogin(t, h, body); rec.Code != http.StatusBadRequest {
				t.Errorf("body %s: status %d, want 400", body, rec.Code)
			}
		}
		if len(c.links) != 0 {
			t.Errorf("sent %d links for bad input", len(c.links))
		}
	})

	t.Run("link is sent and only its hash is stored", func(t *testing.T) {
		var c captureSender
		h := requestLoginLink(pool, c.send, "https://site.test")
		email := testEmail()
		if rec := postLogin(t, h, `{"email": "`+strings.ToUpper(email)+`"}`); rec.Code != http.StatusOK {
			t.Fatalf("status %d, body %s", rec.Code, rec.Body)
		}
		if len(c.links) != 1 {
			t.Fatalf("sent %d links, want 1", len(c.links))
		}
		prefix := "https://site.test/signin.html#token="
		if !strings.HasPrefix(c.links[0], prefix) {
			t.Fatalf("link %q doesn't start with %q", c.links[0], prefix)
		}
		secret := strings.TrimPrefix(c.links[0], prefix)

		var storedEmail string
		var expiresIn time.Duration
		err := pool.QueryRow(ctx, `
			SELECT email, expires_at - created_at FROM login_token WHERE token_hash = $1`,
			hashSecret(secret)).Scan(&storedEmail, &expiresIn)
		if err != nil {
			t.Fatalf("no login_token row for the link's hash: %v", err)
		}
		if storedEmail != email {
			t.Errorf("stored email %q, want lower-cased %q", storedEmail, email)
		}
		if expiresIn != loginTokenTTL {
			t.Errorf("token lives %v, want %v", expiresIn, loginTokenTTL)
		}
	})

	t.Run("rate limit after five links", func(t *testing.T) {
		var c captureSender
		h := requestLoginLink(pool, c.send, "https://site.test")
		body := `{"email": "` + testEmail() + `"}`
		for i := 1; i <= loginLinksPerTTL; i++ {
			if rec := postLogin(t, h, body); rec.Code != http.StatusOK {
				t.Fatalf("request %d: status %d", i, rec.Code)
			}
		}
		if rec := postLogin(t, h, body); rec.Code != http.StatusTooManyRequests {
			t.Fatalf("request %d: status %d, want 429", loginLinksPerTTL+1, rec.Code)
		}
		if len(c.links) != loginLinksPerTTL {
			t.Errorf("sent %d links, want %d", len(c.links), loginLinksPerTTL)
		}
	})

	t.Run("disabled account gets the same answer but no link", func(t *testing.T) {
		email := testEmail()
		_, err := pool.Exec(ctx, `
			INSERT INTO account (id, email, display_name, status) VALUES ($1, $2, 'Disabled test', 'disabled')`,
			newUUIDv7(), email)
		if err != nil {
			t.Fatal(err)
		}
		var c captureSender
		h := requestLoginLink(pool, c.send, "https://site.test")
		rec := postLogin(t, h, `{"email": "`+email+`"}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("status %d, want 200 (same as any other address)", rec.Code)
		}
		if len(c.links) != 0 {
			t.Errorf("sent a link to a disabled account")
		}
	})
}

// newLoginToken stores a token for email that expires after ttl (negative = already
// expired) and returns the raw secret, as if it had come from an emailed link.
func newLoginToken(t *testing.T, pool *pgxpool.Pool, email string, ttl time.Duration) string {
	t.Helper()
	secret := newSecret()
	_, err := pool.Exec(context.Background(), `
		INSERT INTO login_token (id, email, token_hash, created_at, expires_at)
		VALUES ($1, $2, $3, now() - interval '1 hour', now() + $4::int * interval '1 second')`,
		newUUIDv7(), email, hashSecret(secret), int(ttl.Seconds()))
	if err != nil {
		t.Fatal(err)
	}
	return secret
}

func postVerify(t *testing.T, h http.HandlerFunc, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("POST", "/auth/verify", strings.NewReader(body))
	rec := httptest.NewRecorder()
	h(rec, req)
	return rec
}

func TestVerifyLoginToken(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	h := verifyLoginToken(pool)

	t.Run("bad input is rejected", func(t *testing.T) {
		for _, body := range []string{`not json`, `{}`, `{"token": ""}`, `{"token": "` + strings.Repeat("x", 101) + `"}`} {
			if rec := postVerify(t, h, body); rec.Code != http.StatusBadRequest {
				t.Errorf("body %.30s: status %d, want 400", body, rec.Code)
			}
		}
	})

	t.Run("first sign-in creates account and session; token is single-use", func(t *testing.T) {
		email := testEmail()
		token := newLoginToken(t, pool, email, 10*time.Minute)

		rec := postVerify(t, h, `{"token": "`+token+`"}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("status %d, body %s", rec.Code, rec.Body)
		}
		cookies := rec.Result().Cookies()
		if len(cookies) != 1 || cookies[0].Name != sessionCookie {
			t.Fatalf("cookies = %v, want one %q cookie", cookies, sessionCookie)
		}
		c := cookies[0]
		if !c.HttpOnly || !c.Secure || c.SameSite != http.SameSiteLaxMode || c.Path != "/" {
			t.Errorf("cookie flags wrong: %+v", c)
		}

		// The cookie's value is the secret; the database holds only its hash.
		var accountEmail, displayName string
		err := pool.QueryRow(ctx, `
			SELECT a.email, a.display_name FROM web_session s JOIN account a ON a.id = s.account_id
			WHERE s.token_hash = $1 AND s.revoked_at IS NULL AND s.expires_at > now()`,
			hashSecret(c.Value)).Scan(&accountEmail, &displayName)
		if err != nil {
			t.Fatalf("no live web_session for the cookie: %v", err)
		}
		if accountEmail != email || displayName != strings.Split(email, "@")[0] {
			t.Errorf("account = %q / %q", accountEmail, displayName)
		}

		if rec := postVerify(t, h, `{"token": "`+token+`"}`); rec.Code != http.StatusBadRequest {
			t.Errorf("second use: status %d, want 400", rec.Code)
		}
	})

	t.Run("existing account is reused, not duplicated", func(t *testing.T) {
		email := testEmail()
		id := newUUIDv7()
		_, err := pool.Exec(ctx, `INSERT INTO account (id, email, display_name) VALUES ($1, $2, 'Existing')`, id, email)
		if err != nil {
			t.Fatal(err)
		}
		rec := postVerify(t, h, `{"token": "`+newLoginToken(t, pool, strings.ToLower(email), time.Minute)+`"}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("status %d, body %s", rec.Code, rec.Body)
		}
		if !strings.Contains(rec.Body.String(), id) || !strings.Contains(rec.Body.String(), "Existing") {
			t.Errorf("body %s doesn't describe the existing account %s", rec.Body, id)
		}
	})

	t.Run("expired token is rejected", func(t *testing.T) {
		token := newLoginToken(t, pool, testEmail(), -time.Minute)
		if rec := postVerify(t, h, `{"token": "`+token+`"}`); rec.Code != http.StatusBadRequest {
			t.Errorf("status %d, want 400", rec.Code)
		}
	})

	t.Run("disabled account gets no session", func(t *testing.T) {
		email := testEmail()
		_, err := pool.Exec(ctx, `
			INSERT INTO account (id, email, display_name, status) VALUES ($1, $2, 'Disabled', 'disabled')`,
			newUUIDv7(), email)
		if err != nil {
			t.Fatal(err)
		}
		rec := postVerify(t, h, `{"token": "`+newLoginToken(t, pool, email, time.Minute)+`"}`)
		if rec.Code != http.StatusForbidden {
			t.Errorf("status %d, want 403", rec.Code)
		}
		if len(rec.Result().Cookies()) != 0 {
			t.Errorf("disabled account got a cookie")
		}
	})
}
