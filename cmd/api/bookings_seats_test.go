package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func seatReq(pool *pgxpool.Pool, h http.HandlerFunc, cookie *http.Cookie, bookingID, seatID string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("POST", "/bookings/"+bookingID+"/seats", nil)
	req.SetPathValue("id", bookingID)
	if seatID != "" {
		req.SetPathValue("seat", seatID)
	}
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	requireSignIn(pool, h)(rec, req)
	return rec
}

type seatAnswer struct {
	Booking    bookingDTO `json:"booking"`
	DeltaCents int        `json:"delta_cents"`
}

func decodeSeat(t *testing.T, rec *httptest.ResponseRecorder) seatAnswer {
	t.Helper()
	var a seatAnswer
	if err := json.Unmarshal(rec.Body.Bytes(), &a); err != nil || rec.Code != 200 {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	return a
}

// openSeat: one of the booking's open seats.
func openSeat(t *testing.T, pool *pgxpool.Pool, bookingID string) string {
	t.Helper()
	var id string
	if err := pool.QueryRow(context.Background(), `
		SELECT id::text FROM booking_attendee WHERE booking_id = $1 AND status = 'invited' LIMIT 1`, bookingID).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

// More than 24 h ahead: adding and removing seats reprices the booking.
// Prices (60 min, per person): $25 / $20 / $17 / $15 for 1–4 people.
func TestSeatsBeforeLock(t *testing.T) {
	pool := testPool(t)
	teacher, day := privateTeacher(t, pool)
	me := newStudent(t, pool, 10000, false)
	b := decodeBooking(t, bookSeats(pool, me.cookie, newUUIDv7(), teacher, day.Add(19*time.Hour), 1, me.learners...))
	cancelWhenDone(t, pool, b.SessionID)

	steps := []struct {
		name                  string
		add                   bool
		wantSize, wantTotal   int
		wantDelta, wantCredit int
	}{
		{"add: 1 → 2 seats, $25 → $40", true, 2, 4000, 1500, 6000},
		{"add: 2 → 3 seats, $40 → $51", true, 3, 5100, 1100, 4900},
		{"add: 3 → 4 seats, $51 → $60", true, 4, 6000, 900, 4000},
		{"remove an open seat: 4 → 3, back to $51", false, 3, 5100, -900, 4900},
	}
	for _, s := range steps {
		var rec *httptest.ResponseRecorder
		if s.add {
			rec = seatReq(pool, addSeat(pool), me.cookie, b.ID, "")
		} else {
			rec = seatReq(pool, removeSeat(pool), me.cookie, b.ID, openSeat(t, pool, b.ID))
		}
		a := decodeSeat(t, rec)
		if a.Booking.PartySize != s.wantSize || a.Booking.TotalCents != s.wantTotal || a.DeltaCents != s.wantDelta ||
			balance(t, pool, me.accountID) != s.wantCredit {
			t.Errorf("%s: size %d total %d delta %d credit %d", s.name, a.Booking.PartySize, a.Booking.TotalCents,
				a.DeltaCents, balance(t, pool, me.accountID))
		}
	}

	t.Run("refused", func(t *testing.T) {
		decodeSeat(t, seatReq(pool, addSeat(pool), me.cookie, b.ID, "")) // back to 4
		if rec := seatReq(pool, addSeat(pool), me.cookie, b.ID, ""); errorCode(t, rec) != "party_full" {
			t.Errorf("a 5th seat: %d %s", rec.Code, rec.Body)
		}
		other := newStudent(t, pool, 10000, false)
		if rec := seatReq(pool, addSeat(pool), other.cookie, b.ID, ""); rec.Code != 404 {
			t.Errorf("someone else's booking: %d", rec.Code)
		}
		group := bookOK(t, pool, me, newSession(t, pool, newTeacher(t, pool), sessionOpts{slot: 30}))
		if rec := seatReq(pool, addSeat(pool), me.cookie, group.ID, ""); errorCode(t, rec) != "not_private" {
			t.Errorf("a group booking: %d %s", rec.Code, rec.Body)
		}
		if rec := seatReq(pool, removeSeat(pool), me.cookie, b.ID, newUUIDv7()); rec.Code != 404 {
			t.Errorf("an unknown seat: %d", rec.Code)
		}
		poor := newStudent(t, pool, 2500, false)
		pb := decodeBooking(t, bookSeats(pool, poor.cookie, newUUIDv7(), teacher, day.Add(20*time.Hour), 1, poor.learners...))
		cancelWhenDone(t, pool, pb.SessionID)
		if rec := seatReq(pool, addSeat(pool), poor.cookie, pb.ID, ""); errorCode(t, rec) != "insufficient_credit" ||
			count(t, pool, `SELECT party_size FROM booking WHERE id = $1`, pb.ID) != 1 {
			t.Errorf("no credit: %d %s", rec.Code, rec.Body)
		}
		var mySeat string
		pool.QueryRow(context.Background(), `SELECT id FROM booking_attendee WHERE booking_id = $1`, pb.ID).Scan(&mySeat)
		if rec := seatReq(pool, removeSeat(pool), poor.cookie, pb.ID, mySeat); errorCode(t, rec) != "last_seat" {
			t.Errorf("the last seat: %d %s", rec.Code, rec.Body)
		}
	})
}

// Within 24 h: no new seats; removing one gives back 50% of its price (rounded
// up), and the booking keeps its price and party size.
func TestSeatsAfterLock(t *testing.T) {
	pool := testPool(t)
	teacher, _ := privateTeacher(t, pool)
	soon := time.Now().UTC().Truncate(15 * time.Minute).Add(3 * time.Hour)
	addBlock(t, pool, teacher, soon, soon.Add(2*time.Hour))
	me := newStudent(t, pool, 10000, false)
	b := decodeBooking(t, bookSeats(pool, me.cookie, newUUIDv7(), teacher, soon, 3, me.learners...)) // 3 × $17 = $51
	cancelWhenDone(t, pool, b.SessionID)

	if rec := seatReq(pool, addSeat(pool), me.cookie, b.ID, ""); errorCode(t, rec) != "seats_locked" {
		t.Errorf("add within 24 h: %d %s", rec.Code, rec.Body)
	}
	a := decodeSeat(t, seatReq(pool, removeSeat(pool), me.cookie, b.ID, openSeat(t, pool, b.ID)))
	if a.DeltaCents != -850 || a.Booking.PartySize != 3 || a.Booking.TotalCents != 5100 || a.Booking.RefundCents != 850 ||
		a.Booking.OpenSeats != 1 || balance(t, pool, me.accountID) != 10000-5100+850 {
		t.Errorf("remove within 24 h: %+v, delta %d, credit %d; want $8.50 back (half of $17), price and size kept",
			a.Booking, a.DeltaCents, balance(t, pool, me.accountID))
	}
}

// Adding a seat while another booking by the same payer is spending the same
// credit. The test plays the other booking: it locks the account and writes a
// $40 spend, holding the transaction open while the seat is added ($15 more,
// with $50 of credit). With the account lock, the seat change waits, then sees
// $10 left and refuses. Without it, it reads $50, charges $15, and the balance
// ends at -$5.
func TestAddSeatRace(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	teacher, day := privateTeacher(t, pool)
	me := newStudent(t, pool, 7500, false)
	b := decodeBooking(t, bookSeats(pool, me.cookie, newUUIDv7(), teacher, day.Add(19*time.Hour), 1, me.learners...)) // $25, $50 left
	cancelWhenDone(t, pool, b.SessionID)

	gate, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer gate.Rollback(ctx)
	if _, err := gate.Exec(ctx, `SELECT 1 FROM account WHERE id = $1 FOR UPDATE`, me.accountID); err != nil {
		t.Fatal(err)
	}
	if _, err := gate.Exec(ctx, `
		INSERT INTO credit_ledger (id, account_id, entry_type, amount_cents, note)
		VALUES ($1, $2, 'adjustment', -4000, 'stands in for another booking')`, newUUIDv7(), me.accountID); err != nil {
		t.Fatal(err)
	}
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- seatReq(pool, addSeat(pool), me.cookie, b.ID, "") }()
	deadline := time.Now().Add(3 * time.Second)
	for len(done) == 0 && count(t, pool, `SELECT count(*) FROM pg_stat_activity WHERE datname = current_database() AND wait_event_type = 'Lock'`) == 0 &&
		time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if err := gate.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	rec := <-done
	if got := balance(t, pool, me.accountID); got < 0 {
		t.Errorf("balance %d: the seat was charged against credit already spent (%d %s)", got, rec.Code, rec.Body)
	}
	if errorCode(t, rec) != "insufficient_credit" {
		t.Errorf("%d %s, want insufficient_credit", rec.Code, rec.Body)
	}
}
