package main

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

func TestRedirectWWW(t *testing.T) {
	// The "next" handler stands in for the real routes: it just answers 200.
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	h := redirectWWW(next)

	tests := []struct {
		name, host, target string
		wantStatus         int
		wantLocation       string
	}{
		{"www redirects, path and query kept", "www.hablemos.live", "/sessions?kind=group",
			http.StatusPermanentRedirect, "https://hablemos.live/sessions?kind=group"},
		{"www root", "www.hablemos.live", "/", http.StatusPermanentRedirect, "https://hablemos.live/"},
		{"bare domain served", "hablemos.live", "/sessions", http.StatusOK, ""},
		{"run.app address served", "hablemos-api-529979780991.us-east1.run.app", "/health", http.StatusOK, ""},
		{"localhost served", "localhost:8080", "/", http.StatusOK, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest("GET", tt.target, nil)
			req.Host = tt.host
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d", rec.Code, tt.wantStatus)
			}
			if got := rec.Header().Get("Location"); got != tt.wantLocation {
				t.Fatalf("Location = %q, want %q", got, tt.wantLocation)
			}
		})
	}
}

func TestRedactQuery(t *testing.T) {
	tests := []struct {
		name, in, want string
	}{
		{"no query", "", ""},
		{"filters kept", "language=es&audience=kids", "audience=kids&language=es"},
		{"token hidden", "token=abc123", "token=REDACTED"},
		{"any token name, any case", "invite_Token=xyz&kind=group", "invite_Token=REDACTED&kind=group"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			q, err := url.ParseQuery(tt.in)
			if err != nil {
				t.Fatal(err)
			}
			if got := redactQuery(q); got != tt.want {
				t.Fatalf("redactQuery(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}
