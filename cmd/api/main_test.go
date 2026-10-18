package main

import (
	"net/url"
	"testing"
)

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
