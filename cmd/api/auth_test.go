package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
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
