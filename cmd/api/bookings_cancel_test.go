package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// The refund rule on its own: pure arithmetic, no database.
func TestRefundFor(t *testing.T) {
	tests := []struct {
		name       string
		total      int
		untilStart time.Duration
		want       int
	}{
		{"two days ahead: everything back", 1500, 48 * time.Hour, 1500},
		{"one second more than 24h: everything back", 1500, 24*time.Hour + time.Second, 1500},
		{"exactly 24h: half (the rule says more than 24h)", 1500, 24 * time.Hour, 750},
		{"an hour ahead: half", 1500, time.Hour, 750},
		{"odd cents round up for the student", 1525, time.Hour, 763},
		{"one cent: rounds up to one cent", 1, time.Minute, 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := refundFor(tt.total, tt.untilStart); got != tt.want {
				t.Errorf("refundFor(%d, %v) = %d, want %d", tt.total, tt.untilStart, got, tt.want)
			}
		})
	}
}

// cancel sends POST /bookings/{id}/cancel through requireSignIn, as main.go does.
// SetPathValue fills in {id}, which the router would do in the real server.
func cancel(pool *pgxpool.Pool, cookie *http.Cookie, bookingID string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("POST", "/bookings/"+bookingID+"/cancel", nil)
	req.SetPathValue("id", bookingID)
	if cookie != nil {
		req.AddCookie(cookie)
	}
	rec := httptest.NewRecorder()
	requireSignIn(pool, cancelBooking(pool))(rec, req)
	return rec
}

// bookOK books and returns the booking, failing the test if it didn't work.
func bookOK(t *testing.T, pool *pgxpool.Pool, s student, sessionID string) bookingDTO {
	t.Helper()
	rec := book(pool, s.cookie, newUUIDv7(), sessionID, s.learners...)
	if rec.Code != http.StatusCreated {
		t.Fatalf("booking: status %d, body %s", rec.Code, rec.Body)
	}
	return decodeBooking(t, rec)
}

func TestCancelBooking(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	teacher := newTeacher(t, pool)

	t.Run("more than 24h ahead: full refund, written to the ledger and the outbox", func(t *testing.T) {
		s := newStudent(t, pool, 5000, false)
		b := bookOK(t, pool, s, newSession(t, pool, teacher, sessionOpts{slot: 30})) // in 60 hours

		rec := cancel(pool, s.cookie, b.ID)
		if rec.Code != http.StatusOK {
			t.Fatalf("status %d, body %s", rec.Code, rec.Body)
		}
		got := decodeBooking(t, rec)
		if got.Status != "cancelled" || got.CancelledAt == nil || got.RefundCents != 1500 || got.BalanceCents != 5000 {
			t.Errorf("cancelled booking = %+v, want cancelled, refund 1500, balance 5000", got)
		}
		if n := count(t, pool, `SELECT count(*) FROM credit_ledger WHERE booking_id = $1 AND entry_type = 'refund' AND amount_cents = 1500`, b.ID); n != 1 {
			t.Errorf("%d refund entries, want 1", n)
		}
		if n := count(t, pool, `SELECT count(*) FROM outbox_event WHERE aggregate_id = $1 AND event_type = 'booking_cancelled'`, b.ID); n != 1 {
			t.Errorf("%d booking_cancelled events, want 1", n)
		}

		t.Run("cancelling again changes nothing", func(t *testing.T) {
			rec := cancel(pool, s.cookie, b.ID)
			if again := decodeBooking(t, rec); rec.Code != http.StatusOK || again.RefundCents != 1500 || again.BalanceCents != 5000 {
				t.Errorf("status %d, booking %+v; want 200 with the same refund and balance", rec.Code, again)
			}
			if n := count(t, pool, `SELECT count(*) FROM credit_ledger WHERE booking_id = $1 AND entry_type = 'refund'`, b.ID); n != 1 {
				t.Errorf("%d refund entries after the second cancel, want still 1", n)
			}
		})
	})

	t.Run("within 24h: half back", func(t *testing.T) {
		s := newStudent(t, pool, 5000, false)
		b := bookOK(t, pool, s, newSession(t, pool, teacher, sessionOpts{slot: 5})) // in 10 hours
		rec := cancel(pool, s.cookie, b.ID)
		if got := decodeBooking(t, rec); rec.Code != http.StatusOK || got.RefundCents != 750 || got.BalanceCents != 4250 {
			t.Errorf("status %d, booking %+v; want refund 750, balance 4250 (5000 - 1500 + 750)", rec.Code, got)
		}
	})

	t.Run("the freed seat can be booked by someone else", func(t *testing.T) {
		session := newSession(t, pool, teacher, sessionOpts{maxSeats: seats(2), slot: 31})
		a, b, c := newStudent(t, pool, 5000, false), newStudent(t, pool, 5000, false), newStudent(t, pool, 5000, false)
		aBooking := bookOK(t, pool, a, session)
		bookOK(t, pool, b, session)
		if rec := book(pool, c.cookie, "k", session, c.learners...); errorCode(t, rec) != "session_full" {
			t.Fatalf("third booking before the cancel: %d %s, want session_full", rec.Code, rec.Body)
		}
		if rec := cancel(pool, a.cookie, aBooking.ID); rec.Code != http.StatusOK {
			t.Fatalf("cancel: %d %s", rec.Code, rec.Body)
		}
		if rec := book(pool, c.cookie, "k2", session, c.learners...); rec.Code != http.StatusCreated {
			t.Errorf("third booking after the cancel: %d %s, want 201", rec.Code, rec.Body)
		}
	})

	t.Run("a private session is cancelled along with its booking", func(t *testing.T) {
		s := newStudent(t, pool, 0, false)
		private := newSession(t, pool, teacher, sessionOpts{kind: "private", slot: 32})
		// No API books private sessions yet (roadmap step 4), so write the booking directly.
		bookingID := newUUIDv7()
		if _, err := pool.Exec(ctx, `
			INSERT INTO booking (id, session_id, payer_account_id, party_size, price_per_person_cents, total_cents, idempotency_key)
			VALUES ($1, $2, $3, 1, 2000, 2000, 'k')`, bookingID, private, s.accountID); err != nil {
			t.Fatal(err)
		}
		if rec := cancel(pool, s.cookie, bookingID); rec.Code != http.StatusOK {
			t.Fatalf("cancel: %d %s", rec.Code, rec.Body)
		}
		if n := count(t, pool, `SELECT count(*) FROM session WHERE id = $1 AND status = 'cancelled' AND cancel_reason = 'booking_cancelled'`, private); n != 1 {
			t.Error("private session not cancelled with reason booking_cancelled")
		}
	})

	t.Run("after the class started: refused, no refund", func(t *testing.T) {
		s := newStudent(t, pool, 5000, false)
		session := newSession(t, pool, teacher, sessionOpts{slot: 33})
		b := bookOK(t, pool, s, session)
		// Move the class into the past (an hour ago), keeping its columns consistent.
		if _, err := pool.Exec(ctx, `
			UPDATE session SET starts_at = now() - interval '1 hour', ends_at = now(),
			       occupied = tstzrange(now() - interval '1 hour', now() + interval '5 minutes')
			WHERE id = $1`, session); err != nil {
			t.Fatal(err)
		}
		rec := cancel(pool, s.cookie, b.ID)
		if rec.Code != http.StatusConflict || errorCode(t, rec) != "too_late" {
			t.Errorf("status %d, body %s; want 409 too_late", rec.Code, rec.Body)
		}
		if got := balance(t, pool, s.accountID); got != 3500 {
			t.Errorf("balance %d, want 3500 (no refund)", got)
		}
	})

	t.Run("someone else's booking, an unknown one, a bad ID, signed out", func(t *testing.T) {
		owner, stranger := newStudent(t, pool, 5000, false), newStudent(t, pool, 0, false)
		b := bookOK(t, pool, owner, newSession(t, pool, teacher, sessionOpts{slot: 34}))
		for _, tt := range []struct {
			name       string
			cookie     *http.Cookie
			id         string
			wantStatus int
		}{
			{"someone else's", stranger.cookie, b.ID, http.StatusNotFound},
			{"unknown", owner.cookie, newUUIDv7(), http.StatusNotFound},
			{"bad ID", owner.cookie, "abc", http.StatusBadRequest},
			{"signed out", nil, b.ID, http.StatusUnauthorized},
		} {
			if rec := cancel(pool, tt.cookie, tt.id); rec.Code != tt.wantStatus {
				t.Errorf("%s: status %d, want %d", tt.name, rec.Code, tt.wantStatus)
			}
		}
		if got := decodeBooking(t, cancel(pool, owner.cookie, b.ID)); got.Status != "cancelled" {
			t.Errorf("the owner's booking should still be cancellable after those attempts, got %q", got.Status)
		}
	})
}

// Several cancel requests for one booking at the same moment (a frantic
// double-tapper, or retries) must give exactly one refund.
//
// Firing goroutines and hoping they overlap isn't enough here: a cancellation's
// risky moment (between reading "confirmed" and committing) lasts microseconds,
// so they almost never collide and the test would pass even with the locks
// removed. Instead the test makes the overlap certain: it holds the booking row
// locked itself (the "gate"), waits until every request is stuck waiting on a
// lock, then lets them all go at once.
//   - With the code's locks: they queue on the account and booking rows; the
//     first one cancels, every later one finds "cancelled". One refund.
//   - Without them: every request has already read "confirmed" before the gate
//     opens, and each one refunds. (Checked: remove the FOR UPDATEs and this fails.)
func TestCancelBookingRace(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	s := newStudent(t, pool, 5000, false)
	b := bookOK(t, pool, s, newSession(t, pool, newTeacher(t, pool), sessionOpts{slot: 30}))

	// The default pool has only a few connections; each waiting request holds
	// one, so give this test enough for all of them plus the gate.
	const clicks = 5
	big := poolWithConns(t, clicks+3)

	gate, err := big.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer gate.Rollback(ctx)
	if _, err := gate.Exec(ctx, `SELECT 1 FROM booking WHERE id = $1 FOR UPDATE`, b.ID); err != nil {
		t.Fatal(err)
	}

	codes := make(chan int, clicks)
	for range clicks {
		go func() { codes <- cancel(big, s.cookie, b.ID).Code }()
	}
	waitForLockWaiters(t, pool, clicks)
	if err := gate.Rollback(ctx); err != nil { // open the gate
		t.Fatal(err)
	}

	for range clicks {
		if code := <-codes; code != http.StatusOK {
			t.Errorf("a cancel answered %d, want 200", code)
		}
	}
	if n := count(t, pool, `SELECT count(*) FROM credit_ledger WHERE booking_id = $1 AND entry_type = 'refund'`, b.ID); n != 1 {
		t.Errorf("%d refund entries, want exactly 1", n)
	}
	if got := balance(t, pool, s.accountID); got != 5000 {
		t.Errorf("balance %d, want 5000 (refunded once)", got)
	}
}

// poolWithConns is a pool on DATABASE_URL with room for n connections.
func poolWithConns(t *testing.T, n int32) *pgxpool.Pool {
	t.Helper()
	cfg, err := pgxpool.ParseConfig(os.Getenv("DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	cfg.MaxConns = n
	p, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.Close)
	return p
}

// waitForLockWaiters waits until at least n sessions in this database are
// blocked waiting for a lock (pg_stat_activity is Postgres's live view of its
// sessions), or fails after 5 seconds.
func waitForLockWaiters(t *testing.T, pool *pgxpool.Pool, n int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		waiting := count(t, pool, `
			SELECT count(*) FROM pg_stat_activity
			WHERE datname = current_database() AND wait_event_type = 'Lock'`)
		if waiting >= n {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("only %d of %d requests reached the lock in time", waiting, n)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// GET /bookings lists my upcoming confirmed bookings, soonest first, each with
// the refund I'd get if I cancelled now. Cancelled ones and other people's don't appear.
func TestListMyBookings(t *testing.T) {
	pool := testPool(t)
	teacher := newTeacher(t, pool)
	me := newStudent(t, pool, 10000, false, true) // an adult and a child
	other := newStudent(t, pool, 5000, false)

	far := newSession(t, pool, teacher, sessionOpts{audience: "all", minutes: 45, slot: 40}) // in 80h
	near := newSession(t, pool, teacher, sessionOpts{slot: 6})                               // in 12h
	gone := newSession(t, pool, teacher, sessionOpts{slot: 41})

	farBooking := bookOK(t, pool, me, far) // both learners: 2 x 1200
	if rec := book(pool, me.cookie, newUUIDv7(), near, me.learners[0]); rec.Code != http.StatusCreated {
		t.Fatalf("booking near: %d %s", rec.Code, rec.Body)
	}
	cancelled := bookOK(t, pool, student{accountID: me.accountID, cookie: me.cookie, learners: me.learners[:1]}, gone)
	if rec := cancel(pool, me.cookie, cancelled.ID); rec.Code != http.StatusOK {
		t.Fatalf("cancel: %d %s", rec.Code, rec.Body)
	}
	bookOK(t, pool, other, far) // someone else's booking in the same class

	rec := call(requireSignIn(pool, listMyBookings(pool)), "GET", "/bookings", me.cookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d, body %s", rec.Code, rec.Body)
	}
	var got []myBookingDTO
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("%d bookings listed, want 2 (near, far): %+v", len(got), got)
	}
	if got[0].Session.ID != near || got[1].Session.ID != far {
		t.Errorf("order: %s, %s; want near then far", got[0].Session.ID, got[1].Session.ID)
	}
	if got[0].TotalCents != 1500 || got[0].RefundIfCancelledCents != 750 {
		t.Errorf("near (12h away): total %d, refund %d; want 1500 and 750", got[0].TotalCents, got[0].RefundIfCancelledCents)
	}
	f := got[1]
	if f.ID != farBooking.ID || f.TotalCents != 2400 || f.RefundIfCancelledCents != 2400 || f.PartySize != 2 || len(f.Attendees) != 2 {
		t.Errorf("far (80h away) = %+v; want total 2400, full refund, 2 attendees", f)
	}
	if f.Session.DurationMinutes != 45 || f.Session.Audience != "all" {
		t.Errorf("far session = %+v; want 45 min, audience all", f.Session)
	}

	if rec := call(requireSignIn(pool, listMyBookings(pool)), "GET", "/bookings", nil); rec.Code != http.StatusUnauthorized {
		t.Errorf("signed out: status %d, want 401", rec.Code)
	}
}
