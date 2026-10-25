package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// ---------- fixtures: each test builds its own teacher, sessions and students ----------

// newTeacher creates a teacher with group prices for 30/45/60 minutes ($8/$12/$15
// per person, every party size). No 90-minute price, for the no_price case.
func newTeacher(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	ctx := context.Background()
	accountID, _ := newAccount(t, pool)
	teacherID := newUUIDv7()
	if _, err := pool.Exec(ctx, `INSERT INTO teacher (id, account_id) VALUES ($1, $2)`, teacherID, accountID); err != nil {
		t.Fatal(err)
	}
	retireWhenDone(t, pool, teacherID)
	for _, p := range []struct{ minutes, cents int }{{30, 800}, {45, 1200}, {60, 1500}} {
		for size := 1; size <= 4; size++ {
			_, err := pool.Exec(ctx, `
				INSERT INTO price_tier (id, teacher_id, kind, duration_minutes, party_size, price_per_person_cents)
				VALUES ($1, $2, 'group', $3, $4, $5)`, newUUIDv7(), teacherID, p.minutes, size, p.cents)
			if err != nil {
				t.Fatal(err)
			}
		}
	}
	return teacherID
}

type sessionOpts struct {
	kind, audience string
	maxSeats       *int // nil = no cap
	slot           int  // starts in slot*2 hours; negative = already started
	minutes        int
}

// newSession creates a session for the teacher. Slots are 2 hours apart; give
// each session of one teacher its own slot (the no-double-booking constraint applies).
func newSession(t *testing.T, pool *pgxpool.Pool, teacherID string, o sessionOpts) string {
	t.Helper()
	if o.kind == "" {
		o.kind = "group"
	}
	if o.audience == "" {
		o.audience = "adults"
	}
	if o.slot == 0 {
		o.slot = 48
	}
	if o.minutes == 0 {
		o.minutes = 60
	}
	minSeats := 2
	if o.kind == "private" {
		minSeats = 1
	}
	id := newUUIDv7()
	_, err := pool.Exec(context.Background(), `
		INSERT INTO session (id, teacher_id, kind, target_language, audience, title,
		                     starts_at, ends_at, occupied, min_seats, max_seats)
		SELECT $1, $2, $3, 'es', $4, 'Test session', s, s + $6 * interval '1 minute',
		       tstzrange(s, s + $6 * interval '1 minute'), $7, $8 -- occupied = the class itself (break is inside)
		FROM (SELECT date_trunc('minute', now()) + $5 * interval '2 hours' AS s) AS t`,
		id, teacherID, o.kind, o.audience, o.slot, o.minutes, minSeats, o.maxSeats)
	if err != nil {
		t.Fatal(err)
	}
	cancelWhenDone(t, pool, id)
	return id
}

// retireWhenDone marks a test's teacher inactive when the test ends, so test
// teachers (and their availability) don't show up in the dev database's
// private-class slots. A status change, not a delete, like everything else.
func retireWhenDone(t *testing.T, pool *pgxpool.Pool, teacherID string) {
	t.Cleanup(func() {
		if _, err := pool.Exec(context.Background(), `UPDATE teacher SET status = 'inactive' WHERE id = $1`, teacherID); err != nil {
			t.Errorf("retiring test teacher: %v", err)
		}
	})
}

// cancelWhenDone cancels a test's session when the test ends, so test sessions
// don't pile up in the dev database's catalog (or crowd real ones out of the
// 100 that GET /sessions returns). Cancelled, not deleted: business records
// are never deleted.
func cancelWhenDone(t *testing.T, pool *pgxpool.Pool, sessionID string) {
	t.Cleanup(func() {
		_, err := pool.Exec(context.Background(), `
			UPDATE session SET status = 'cancelled', cancel_reason = 'teacher_cancelled'
			WHERE id = $1 AND status <> 'cancelled'`, sessionID)
		if err != nil {
			t.Errorf("cancelling test session: %v", err)
		}
	})
}

type student struct {
	accountID string
	cookie    *http.Cookie
	learners  []string
}

// newStudent creates a signed-in account with the given credit, and one
// learner per entry in minors (true = a child).
func newStudent(t *testing.T, pool *pgxpool.Pool, creditCents int, minors ...bool) student {
	t.Helper()
	ctx := context.Background()
	id, email := newAccount(t, pool)
	s := student{accountID: id, cookie: signIn(t, pool, email)}
	holder := true // the first adult is the account holder, as at a real first sign-in
	for _, minor := range minors {
		lid := newUUIDv7()
		_, err := pool.Exec(ctx, `
			INSERT INTO learner (id, account_id, display_name, native_language, is_minor, is_account_holder)
			VALUES ($1, $2, 'Learner', 'en', $3, $4)`, lid, id, minor, holder && !minor)
		holder = holder && minor
		if err != nil {
			t.Fatal(err)
		}
		s.learners = append(s.learners, lid)
	}
	if creditCents > 0 {
		_, err := pool.Exec(ctx, `
			INSERT INTO credit_ledger (id, account_id, entry_type, amount_cents, note)
			VALUES ($1, $2, 'adjustment', $3, 'test credit')`, newUUIDv7(), id, creditCents)
		if err != nil {
			t.Fatal(err)
		}
	}
	return s
}

func seats(n int) *int { return &n }

// book sends POST /bookings through requireSignIn, as main.go does.
func book(pool *pgxpool.Pool, cookie *http.Cookie, key, sessionID string, learnerIDs ...string) *httptest.ResponseRecorder {
	body, _ := json.Marshal(bookingRequest{SessionID: sessionID, LearnerIDs: learnerIDs})
	req := httptest.NewRequest("POST", "/bookings", strings.NewReader(string(body)))
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
	}
	if cookie != nil {
		req.AddCookie(cookie)
	}
	rec := httptest.NewRecorder()
	requireSignIn(pool, createBooking(pool))(rec, req)
	return rec
}

// errorCode returns the "code" field of an error response.
func errorCode(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var body struct{ Code string }
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body is not JSON: %s", rec.Body)
	}
	return body.Code
}

func decodeBooking(t *testing.T, rec *httptest.ResponseRecorder) bookingDTO {
	t.Helper()
	var b bookingDTO
	if err := json.Unmarshal(rec.Body.Bytes(), &b); err != nil {
		t.Fatalf("body is not a booking: %s", rec.Body)
	}
	return b
}

// count runs a SELECT count(*) style query.
func count(t *testing.T, pool *pgxpool.Pool, sql string, args ...any) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func balance(t *testing.T, pool *pgxpool.Pool, accountID string) int {
	return count(t, pool, `SELECT COALESCE(SUM(amount_cents), 0) FROM credit_ledger WHERE account_id = $1`, accountID)
}

// ---------- the tests ----------

func TestCreateBooking(t *testing.T) {
	pool := testPool(t)
	teacher := newTeacher(t, pool)

	t.Run("books, debits credit, writes attendees and an outbox event", func(t *testing.T) {
		session := newSession(t, pool, teacher, sessionOpts{maxSeats: seats(8), slot: 10})
		s := newStudent(t, pool, 5000, false)

		rec := book(pool, s.cookie, "k1", session, s.learners...)
		if rec.Code != http.StatusCreated {
			t.Fatalf("status %d, body %s", rec.Code, rec.Body)
		}
		b := decodeBooking(t, rec)
		if b.TotalCents != 1500 || b.PricePerPersonCents != 1500 || b.PartySize != 1 || b.BalanceCents != 3500 {
			t.Errorf("booking = %+v", b)
		}
		if got := balance(t, pool, s.accountID); got != 3500 {
			t.Errorf("balance %d, want 3500", got)
		}
		if n := count(t, pool, `SELECT count(*) FROM credit_ledger WHERE booking_id = $1 AND entry_type = 'spend' AND amount_cents = -1500`, b.ID); n != 1 {
			t.Errorf("%d spend entries, want 1", n)
		}
		if n := count(t, pool, `SELECT count(*) FROM booking_attendee WHERE booking_id = $1 AND learner_id = $2 AND status = 'joined'`, b.ID, s.learners[0]); n != 1 {
			t.Errorf("%d attendee rows, want 1", n)
		}
		if n := count(t, pool, `SELECT count(*) FROM outbox_event WHERE aggregate_id = $1 AND event_type = 'booking_created' AND processed_at IS NULL`, b.ID); n != 1 {
			t.Errorf("%d outbox events, want 1", n)
		}

		t.Run("same key again: same booking, no second charge", func(t *testing.T) {
			rec := book(pool, s.cookie, "k1", session, s.learners...)
			if rec.Code != http.StatusOK {
				t.Fatalf("status %d, body %s", rec.Code, rec.Body)
			}
			if again := decodeBooking(t, rec); again.ID != b.ID || again.BalanceCents != 3500 {
				t.Errorf("replay = %+v, want booking %s with balance 3500", again, b.ID)
			}
			if n := count(t, pool, `SELECT count(*) FROM booking WHERE payer_account_id = $1`, s.accountID); n != 1 {
				t.Errorf("%d bookings, want 1", n)
			}
		})

		t.Run("same key, different request: refused", func(t *testing.T) {
			other := newSession(t, pool, teacher, sessionOpts{slot: 11})
			rec := book(pool, s.cookie, "k1", other, s.learners...)
			if rec.Code != http.StatusUnprocessableEntity || errorCode(t, rec) != "idempotency_key_reused" {
				t.Errorf("status %d, body %s", rec.Code, rec.Body)
			}
		})

		t.Run("same learner again, new key: already booked", func(t *testing.T) {
			rec := book(pool, s.cookie, "k2", session, s.learners...)
			if rec.Code != http.StatusConflict || errorCode(t, rec) != "already_booked" {
				t.Errorf("status %d, body %s", rec.Code, rec.Body)
			}
		})
	})

	t.Run("a party of two pays twice the per-person price", func(t *testing.T) {
		session := newSession(t, pool, teacher, sessionOpts{audience: "all", minutes: 45, slot: 12})
		s := newStudent(t, pool, 5000, false, true) // a parent and a child
		rec := book(pool, s.cookie, "k", session, s.learners...)
		if rec.Code != http.StatusCreated {
			t.Fatalf("status %d, body %s", rec.Code, rec.Body)
		}
		if b := decodeBooking(t, rec); b.TotalCents != 2400 || b.PartySize != 2 {
			t.Errorf("booking = %+v, want 2 x 1200", b)
		}
	})

	// Cases that must be refused, with nothing written.
	full := newSession(t, pool, teacher, sessionOpts{maxSeats: seats(2), slot: 13})
	for _, s := range []student{newStudent(t, pool, 5000, false), newStudent(t, pool, 5000, false)} {
		if rec := book(pool, s.cookie, "k", full, s.learners...); rec.Code != http.StatusCreated {
			t.Fatalf("filling the session: status %d, body %s", rec.Code, rec.Body)
		}
	}
	other := newStudent(t, pool, 1000, false) // would also be rejected for credit
	stranger := newStudent(t, pool, 0, false)

	tests := []struct {
		name       string
		session    string
		student    student
		learners   []string
		key        string
		wantStatus int
		wantCode   string
	}{
		{"not enough credit", newSession(t, pool, teacher, sessionOpts{slot: 14}), newStudent(t, pool, 1000, false), nil, "k",
			http.StatusConflict, "insufficient_credit"},
		{"no seats left", full, newStudent(t, pool, 5000, false), nil, "k",
			http.StatusConflict, "session_full"},
		{"child in an adults session", newSession(t, pool, teacher, sessionOpts{slot: 15}), newStudent(t, pool, 5000, true), nil, "k",
			http.StatusConflict, "audience_mismatch"},
		{"adult in a kids session", newSession(t, pool, teacher, sessionOpts{audience: "kids", minutes: 30, slot: 16}), newStudent(t, pool, 5000, false), nil, "k",
			http.StatusConflict, "audience_mismatch"},
		{"session already started", newSession(t, pool, teacher, sessionOpts{slot: -1}), newStudent(t, pool, 5000, false), nil, "k",
			http.StatusConflict, "session_not_open"},
		{"private session", newSession(t, pool, teacher, sessionOpts{kind: "private", slot: 17}), newStudent(t, pool, 5000, false), nil, "k",
			http.StatusConflict, "session_not_open"},
		{"no price for 90 minutes", newSession(t, pool, teacher, sessionOpts{minutes: 90, slot: 18}), newStudent(t, pool, 5000, false), nil, "k",
			http.StatusConflict, "no_price"},
		{"unknown session", newUUIDv7(), newStudent(t, pool, 5000, false), nil, "k",
			http.StatusNotFound, "session_not_found"},
		{"someone else's learner", newSession(t, pool, teacher, sessionOpts{slot: 19}), stranger, other.learners, "k",
			http.StatusBadRequest, "unknown_learner"},
		{"no Idempotency-Key", newSession(t, pool, teacher, sessionOpts{slot: 20}), newStudent(t, pool, 5000, false), nil, "",
			http.StatusBadRequest, ""},
		{"not a session ID", "abc", newStudent(t, pool, 5000, false), nil, "k",
			http.StatusBadRequest, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			learners := tt.learners
			if learners == nil {
				learners = tt.student.learners
			}
			before := balance(t, pool, tt.student.accountID)
			rec := book(pool, tt.student.cookie, tt.key, tt.session, learners...)
			if rec.Code != tt.wantStatus || errorCode(t, rec) != tt.wantCode {
				t.Errorf("status %d, body %s; want %d %q", rec.Code, rec.Body, tt.wantStatus, tt.wantCode)
			}
			if n := count(t, pool, `SELECT count(*) FROM booking WHERE payer_account_id = $1`, tt.student.accountID); n != 0 {
				t.Errorf("%d bookings written, want 0", n)
			}
			if after := balance(t, pool, tt.student.accountID); after != before {
				t.Errorf("balance changed from %d to %d", before, after)
			}
		})
	}

	t.Run("signed out: 401", func(t *testing.T) {
		if rec := book(pool, nil, "k", full, newUUIDv7()); rec.Code != http.StatusUnauthorized {
			t.Errorf("status %d, want 401", rec.Code)
		}
	})
}

// TestBookingRaces fires requests at the same moment to prove the row locks
// work: the database, not luck, decides who wins.
func TestBookingRaces(t *testing.T) {
	pool := testPool(t)
	teacher := newTeacher(t, pool)

	// race runs each function in its own goroutine at once and returns the statuses.
	race := func(fns ...func() int) map[int]int {
		var wg sync.WaitGroup
		var mu sync.Mutex
		statuses := map[int]int{}
		start := make(chan struct{})
		for _, fn := range fns {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start // all goroutines wait here, then go together
				code := fn()
				mu.Lock()
				statuses[code]++
				mu.Unlock()
			}()
		}
		close(start)
		wg.Wait()
		return statuses
	}

	t.Run("six people, two seats: exactly two get in", func(t *testing.T) {
		session := newSession(t, pool, teacher, sessionOpts{maxSeats: seats(2), slot: 30})
		var fns []func() int
		for range 6 {
			s := newStudent(t, pool, 5000, false)
			fns = append(fns, func() int { return book(pool, s.cookie, "k", session, s.learners...).Code })
		}
		got := race(fns...)
		if got[http.StatusCreated] != 2 || got[http.StatusConflict] != 4 {
			t.Errorf("statuses %v, want 2 x 201 and 4 x 409", got)
		}
		if n := count(t, pool, `SELECT COALESCE(SUM(party_size), 0) FROM booking WHERE session_id = $1 AND status = 'confirmed'`, session); n != 2 {
			t.Errorf("%d seats booked, want 2", n)
		}
	})

	t.Run("credit for one class, two tabs booking different classes: one wins", func(t *testing.T) {
		s := newStudent(t, pool, 1500, false)
		a := newSession(t, pool, teacher, sessionOpts{slot: 31})
		b := newSession(t, pool, teacher, sessionOpts{slot: 32})
		got := race(
			func() int { return book(pool, s.cookie, "tab-a", a, s.learners...).Code },
			func() int { return book(pool, s.cookie, "tab-b", b, s.learners...).Code },
		)
		if got[http.StatusCreated] != 1 || got[http.StatusConflict] != 1 {
			t.Errorf("statuses %v, want one 201 and one 409", got)
		}
		if bal := balance(t, pool, s.accountID); bal != 0 {
			t.Errorf("balance %d, want 0 (and never negative)", bal)
		}
	})

	t.Run("the same click sent three times at once: one booking", func(t *testing.T) {
		s := newStudent(t, pool, 5000, false)
		session := newSession(t, pool, teacher, sessionOpts{slot: 33})
		click := func() int { return book(pool, s.cookie, "same-click", session, s.learners...).Code }
		got := race(click, click, click)
		if got[http.StatusCreated] != 1 || got[http.StatusOK] != 2 {
			t.Errorf("statuses %v, want one 201 and two 200", got)
		}
		if bal := balance(t, pool, s.accountID); bal != 3500 {
			t.Errorf("balance %d, want 3500 (charged once)", bal)
		}
	})
}
