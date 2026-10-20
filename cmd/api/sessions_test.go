package main

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// newAccount inserts an account with a fresh email and returns its ID and email.
func newAccount(t *testing.T, pool *pgxpool.Pool) (id, email string) {
	t.Helper()
	id, email = newUUIDv7(), testEmail()
	_, err := pool.Exec(context.Background(),
		`INSERT INTO account (id, email, display_name) VALUES ($1, $2, 'Test')`, id, email)
	if err != nil {
		t.Fatal(err)
	}
	return id, email
}

// listedIDs calls GET /sessions (wrapped in signedIn, as in main.go) and returns
// the IDs it listed, or the status code if it wasn't 200.
func listedIDs(t *testing.T, pool *pgxpool.Pool, query string, cookie *http.Cookie) (map[string]bool, int) {
	t.Helper()
	rec := call(signedIn(pool, listSessions(pool)), "GET", "/sessions"+query, cookie)
	if rec.Code != http.StatusOK {
		return nil, rec.Code
	}
	var sessions []sessionDTO
	if err := json.NewDecoder(rec.Body).Decode(&sessions); err != nil {
		t.Fatal(err)
	}
	ids := map[string]bool{}
	for _, s := range sessions {
		ids[s.ID] = true
	}
	return ids, http.StatusOK
}

func TestListSessionsPrivacy(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatal(err)
		}
	}

	// A fresh teacher, so these sessions can't overlap anyone else's (exclusion constraint).
	teacherAcct, teacherEmail := newAccount(t, pool)
	teacherID := newUUIDv7()
	exec(`INSERT INTO teacher (id, account_id) VALUES ($1, $2)`, teacherID, teacherAcct)

	groupID, privateID := newUUIDv7(), newUUIDv7()
	exec(`INSERT INTO session (id, teacher_id, kind, target_language, title, starts_at, ends_at, occupied, min_seats)
	      VALUES ($1, $2, 'group', 'es', 'Test group', now() + interval '10 days', now() + interval '10 days 1 hour',
	              tstzrange(now() + interval '10 days', now() + interval '10 days 1 hour'), 2)`, groupID, teacherID)
	exec(`INSERT INTO session (id, teacher_id, kind, target_language, starts_at, ends_at, occupied)
	      VALUES ($1, $2, 'private', 'es', now() + interval '11 days', now() + interval '11 days 1 hour',
	              tstzrange(now() + interval '11 days', now() + interval '11 days 1 hour'))`, privateID, teacherID)

	// The payer books a party of 2; a friend (another account) joins with their learner.
	payerAcct, payerEmail := newAccount(t, pool)
	friendAcct, friendEmail := newAccount(t, pool)
	_, strangerEmail := newAccount(t, pool)
	bookingID, friendLearner := newUUIDv7(), newUUIDv7()
	exec(`INSERT INTO booking (id, session_id, payer_account_id, party_size, price_per_person_cents, total_cents, idempotency_key)
	      VALUES ($1, $2, $3, 2, 1000, 2000, $4)`, bookingID, privateID, payerAcct, newUUIDv7())
	exec(`INSERT INTO learner (id, account_id, display_name, native_language) VALUES ($1, $2, 'Friend', 'en')`,
		friendLearner, friendAcct)
	exec(`INSERT INTO booking_attendee (id, booking_id, learner_id) VALUES ($1, $2, $3)`,
		newUUIDv7(), bookingID, friendLearner)

	t.Run("who sees the private session", func(t *testing.T) {
		tests := []struct {
			name  string
			email string // "" = signed out
			sees  bool
		}{
			{"signed out", "", false},
			{"stranger", strangerEmail, false},
			{"teacher", teacherEmail, true},
			{"payer", payerEmail, true},
			{"friend in the party", friendEmail, true},
		}
		for _, tt := range tests {
			var cookie *http.Cookie
			if tt.email != "" {
				cookie = signIn(t, pool, tt.email)
			}
			ids, code := listedIDs(t, pool, "", cookie)
			if code != http.StatusOK {
				t.Fatalf("%s: status %d", tt.name, code)
			}
			if !ids[groupID] {
				t.Errorf("%s: group session missing; group sessions are public", tt.name)
			}
			if ids[privateID] != tt.sees {
				t.Errorf("%s: sees private session = %v, want %v", tt.name, ids[privateID], tt.sees)
			}
		}
	})

	t.Run("kind=private needs a sign-in", func(t *testing.T) {
		if _, code := listedIDs(t, pool, "?kind=private", nil); code != http.StatusUnauthorized {
			t.Errorf("status %d, want 401", code)
		}
		ids, code := listedIDs(t, pool, "?kind=private", signIn(t, pool, payerEmail))
		if code != http.StatusOK || !ids[privateID] || ids[groupID] {
			t.Errorf("payer's kind=private list: status %d, ids %v", code, ids)
		}
	})

	t.Run("cancelled booking no longer grants access", func(t *testing.T) {
		exec(`UPDATE booking SET status = 'cancelled', cancelled_at = now() WHERE id = $1`, bookingID)
		ids, _ := listedIDs(t, pool, "", signIn(t, pool, payerEmail))
		if ids[privateID] {
			t.Error("payer still sees the session after cancelling")
		}
	})

	t.Run("me says who is a teacher", func(t *testing.T) {
		for _, tt := range []struct {
			email string
			want  bool
		}{{teacherEmail, true}, {payerEmail, false}} {
			rec := call(requireSignIn(pool, getMe(pool)), "GET", "/me", signIn(t, pool, tt.email))
			var body struct{ Account account }
			if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			if body.Account.IsTeacher != tt.want {
				t.Errorf("%s: is_teacher = %v, want %v", tt.email, body.Account.IsTeacher, tt.want)
			}
		}
	})
}
