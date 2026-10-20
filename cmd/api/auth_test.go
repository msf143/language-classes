package main

import (
	"context"
	"encoding/json"
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

// signIn runs a fresh login token through verifyLoginToken and returns the
// session cookie, as a browser would hold it after clicking "Sign in".
func signIn(t *testing.T, pool *pgxpool.Pool, email string) *http.Cookie {
	t.Helper()
	rec := postVerify(t, verifyLoginToken(pool), `{"token": "`+newLoginToken(t, pool, email, time.Minute)+`"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("sign-in: status %d, body %s", rec.Code, rec.Body)
	}
	return rec.Result().Cookies()[0]
}

// call sends a request to h, with the cookie if it isn't nil.
func call(h http.HandlerFunc, method, path string, cookie *http.Cookie) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, nil)
	if cookie != nil {
		req.AddCookie(cookie)
	}
	rec := httptest.NewRecorder()
	h(rec, req)
	return rec
}

func TestSignedIn(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	me := requireSignIn(pool, getMe(pool))

	t.Run("signed out: 401 without a cookie or with an unknown one", func(t *testing.T) {
		for _, c := range []*http.Cookie{nil, {Name: sessionCookie, Value: newSecret()}} {
			if rec := call(me, "GET", "/me", c); rec.Code != http.StatusUnauthorized {
				t.Errorf("cookie %v: status %d, want 401", c, rec.Code)
			}
		}
	})

	t.Run("signedIn lets signed-out requests through without an account", func(t *testing.T) {
		called := false
		h := signedIn(pool, func(w http.ResponseWriter, r *http.Request) {
			called = true
			if _, ok := accountFrom(r); ok {
				t.Error("found an account without a cookie")
			}
		})
		call(h, "GET", "/", nil)
		if !called {
			t.Error("handler not called")
		}
	})

	t.Run("me, logout, then the old cookie no longer works", func(t *testing.T) {
		email := testEmail()
		cookie := signIn(t, pool, email)

		rec := call(me, "GET", "/me", cookie)
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), email) {
			t.Fatalf("GET /me: status %d, body %s", rec.Code, rec.Body)
		}

		rec = call(logout(pool), "POST", "/auth/logout", cookie)
		if rec.Code != http.StatusOK {
			t.Fatalf("logout: status %d", rec.Code)
		}
		if cs := rec.Result().Cookies(); len(cs) != 1 || cs[0].Name != sessionCookie || cs[0].MaxAge >= 0 {
			t.Errorf("logout should delete the cookie, got %v", cs)
		}
		var revoked bool
		err := pool.QueryRow(ctx, `SELECT revoked_at IS NOT NULL FROM web_session WHERE token_hash = $1`,
			hashSecret(cookie.Value)).Scan(&revoked)
		if err != nil || !revoked {
			t.Errorf("web_session not revoked (err %v)", err)
		}

		// Someone who kept a copy of the cookie can't use it after logout.
		if rec := call(me, "GET", "/me", cookie); rec.Code != http.StatusUnauthorized {
			t.Errorf("GET /me after logout: status %d, want 401", rec.Code)
		}
	})

	t.Run("logout when signed out still succeeds", func(t *testing.T) {
		if rec := call(logout(pool), "POST", "/auth/logout", nil); rec.Code != http.StatusOK {
			t.Errorf("status %d, want 200", rec.Code)
		}
	})

	t.Run("expired session is signed out", func(t *testing.T) {
		cookie := signIn(t, pool, testEmail())
		_, err := pool.Exec(ctx, `
			UPDATE web_session SET created_at = now() - interval '2 days', expires_at = now() - interval '1 day'
			WHERE token_hash = $1`, hashSecret(cookie.Value))
		if err != nil {
			t.Fatal(err)
		}
		if rec := call(me, "GET", "/me", cookie); rec.Code != http.StatusUnauthorized {
			t.Errorf("status %d, want 401", rec.Code)
		}
	})

	t.Run("disabling an account signs it out everywhere", func(t *testing.T) {
		email := testEmail()
		cookie := signIn(t, pool, email)
		if _, err := pool.Exec(ctx, `UPDATE account SET status = 'disabled' WHERE email = $1`, email); err != nil {
			t.Fatal(err)
		}
		if rec := call(me, "GET", "/me", cookie); rec.Code != http.StatusUnauthorized {
			t.Errorf("status %d, want 401", rec.Code)
		}
	})
}

// GET /me also returns the credit balance and the active learners (for the Book dialog).
func TestGetMeBalanceAndLearners(t *testing.T) {
	pool := testPool(t)
	s := newStudent(t, pool, 2500, false, true) // $25, an adult learner and a child
	if _, err := pool.Exec(context.Background(), `
		INSERT INTO learner (id, account_id, display_name, native_language, status)
		VALUES ($1, $2, 'Archived', 'en', 'archived')`, newUUIDv7(), s.accountID); err != nil {
		t.Fatal(err)
	}

	rec := call(requireSignIn(pool, getMe(pool)), "GET", "/me", s.cookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d, body %s", rec.Code, rec.Body)
	}
	var me struct {
		BalanceCents int          `json:"balance_cents"`
		Currency     string       `json:"currency"`
		Learners     []learnerDTO `json:"learners"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &me); err != nil {
		t.Fatal(err)
	}
	if me.BalanceCents != 2500 || me.Currency != "USD" {
		t.Errorf("balance %d %s, want 2500 USD", me.BalanceCents, me.Currency)
	}
	if len(me.Learners) != 2 || me.Learners[0].ID != s.learners[0] || me.Learners[0].IsMinor ||
		me.Learners[1].ID != s.learners[1] || !me.Learners[1].IsMinor {
		t.Errorf("learners = %+v, want the adult then the child (not the archived one)", me.Learners)
	}
}

// The first sign-in creates the account and a learner for the account holder;
// signing in again doesn't add another.
func TestFirstSignInCreatesLearner(t *testing.T) {
	pool := testPool(t)
	email := testEmail() // no account yet
	learners := func() int {
		return count(t, pool, `
			SELECT count(*) FROM learner l JOIN account a ON a.id = l.account_id
			WHERE lower(a.email) = lower($1) AND l.status = 'active' AND NOT l.is_minor
			  AND l.display_name = a.display_name`, email)
	}

	signIn(t, pool, email)
	if n := learners(); n != 1 {
		t.Fatalf("after first sign-in: %d learners, want 1", n)
	}
	signIn(t, pool, email)
	if n := learners(); n != 1 {
		t.Errorf("after second sign-in: %d learners, want still 1", n)
	}
}
