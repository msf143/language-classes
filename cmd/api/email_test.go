package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestResendSender points the sender at a fake Resend (an httptest server) and
// checks the request it makes and how it handles the answer.
func TestResendSender(t *testing.T) {
	const key = "re_test_key"
	const link = "https://hablemos.live/signin.html#token=abc123"

	tests := []struct {
		name      string
		status    int
		answer    string
		wantError bool
	}{
		{"sent", http.StatusOK, `{"id":"49a3999c-0ce1-4ea6-ab68-afcd6dc2e794"}`, false},
		{"rejected", http.StatusUnprocessableEntity, `{"statusCode":422,"message":"domain is not verified"}`, true},
		{"resend down", http.StatusServiceUnavailable, ``, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got struct {
				method, path, auth, contentType string
				body                            map[string]any
			}
			fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				got.method, got.path = r.Method, r.URL.Path
				got.auth, got.contentType = r.Header.Get("Authorization"), r.Header.Get("Content-Type")
				if err := json.NewDecoder(r.Body).Decode(&got.body); err != nil {
					t.Errorf("request body is not JSON: %v", err)
				}
				w.WriteHeader(tt.status)
				w.Write([]byte(tt.answer))
			}))
			defer fake.Close()

			send := resendSender(key, fake.URL+"/emails")
			err := send(httptest.NewRequest("POST", "/auth/login", nil), "ana@example.com", link)

			if (err != nil) != tt.wantError {
				t.Fatalf("error = %v, wantError %v", err, tt.wantError)
			}
			if err != nil && strings.Contains(err.Error(), key) {
				t.Fatalf("error message leaks the API key: %v", err)
			}
			if got.method != "POST" || got.path != "/emails" {
				t.Errorf("request = %s %s, want POST /emails", got.method, got.path)
			}
			if got.auth != "Bearer "+key || got.contentType != "application/json" {
				t.Errorf("headers: Authorization %q, Content-Type %q", got.auth, got.contentType)
			}
			if to, _ := got.body["to"].([]any); len(to) != 1 || to[0] != "ana@example.com" {
				t.Errorf("to = %v", got.body["to"])
			}
			if got.body["from"] != loginEmailFrom {
				t.Errorf("from = %v", got.body["from"])
			}
			text, _ := got.body["text"].(string)
			for _, want := range []string{link, "15 minutes", "15 minutos"} {
				if !strings.Contains(text, want) {
					t.Errorf("email text lacks %q:\n%s", want, text)
				}
			}
		})
	}
}
