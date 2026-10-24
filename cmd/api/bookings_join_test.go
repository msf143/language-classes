package main

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// seatLinkToken: a fresh link for the seat; returns the secret after "#token=".
func seatLinkToken(t *testing.T, pool *pgxpool.Pool, payer student, bookingID, seatID string) string {
	t.Helper()
	rec := seatReq(pool, seatLink(pool, "https://hablemos.live"), payer.cookie, bookingID, seatID)
	var a struct{ Link string }
	json.Unmarshal(rec.Body.Bytes(), &a)
	if rec.Code != 200 || !strings.HasPrefix(a.Link, "https://hablemos.live/join.html#token=") {
		t.Fatalf("link: %d %s", rec.Code, rec.Body)
	}
	return strings.TrimPrefix(a.Link, "https://hablemos.live/join.html#token=")
}

func preview(pool *pgxpool.Pool, token string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("POST", "/seats/preview", strings.NewReader(`{"token": "`+token+`"}`))
	rec := httptest.NewRecorder()
	seatPreview(pool)(rec, req)
	return rec
}

func join(pool *pgxpool.Pool, s student, token, learnerID string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("POST", "/seats/join", strings.NewReader(`{"token": "`+token+`", "learner_id": "`+learnerID+`"}`))
	req.AddCookie(s.cookie)
	rec := httptest.NewRecorder()
	requireSignIn(pool, joinSeat(pool))(rec, req)
	return rec
}

func leave(pool *pgxpool.Pool, s student, seatID string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("POST", "/seats/"+seatID+"/leave", nil)
	req.SetPathValue("seat", seatID)
	req.AddCookie(s.cookie)
	rec := httptest.NewRecorder()
	requireSignIn(pool, leaveSeat(pool))(rec, req)
	return rec
}

// privateWithOpenSeats: a payer's private booking of 3 seats (just themself),
// so 2 open; returns the payer, the booking and the open seat IDs.
func privateWithOpenSeats(t *testing.T, pool *pgxpool.Pool, hour int) (student, bookingDTO, []string) {
	t.Helper()
	teacher, day := privateTeacher(t, pool)
	payer := newStudent(t, pool, 10000, false)
	b := decodeBooking(t, bookSeats(pool, payer.cookie, newUUIDv7(), teacher, day.Add(time.Duration(hour)*time.Hour), 3, payer.learners...))
	cancelWhenDone(t, pool, b.SessionID)
	rows, _ := pool.Query(context.Background(), `
		SELECT id::text FROM booking_attendee WHERE booking_id = $1 AND status = 'invited' ORDER BY id`, b.ID)
	var seats []string
	for rows.Next() {
		var id string
		rows.Scan(&id)
		seats = append(seats, id)
	}
	return payer, b, seats
}

func TestJoinSeat(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	payer, b, seats := privateWithOpenSeats(t, pool, 19)
	friend := newStudent(t, pool, 0, false)

	t.Run("a fresh link replaces the old one", func(t *testing.T) {
		old := seatLinkToken(t, pool, payer, b.ID, seats[0])
		_ = seatLinkToken(t, pool, payer, b.ID, seats[0])
		if rec := preview(pool, old); rec.Code != 404 {
			t.Errorf("the old link still works: %d", rec.Code)
		}
	})
	token := seatLinkToken(t, pool, payer, b.ID, seats[0])

	t.Run("preview: the class and who invited you, before signing in", func(t *testing.T) {
		rec := preview(pool, token)
		var p seatPreviewDTO
		json.Unmarshal(rec.Body.Bytes(), &p)
		if rec.Code != 200 || p.Kind != "private" || p.DurationMinutes != 60 || p.InvitedBy != "Test" || p.TeacherName == "" {
			t.Errorf("%d %s", rec.Code, rec.Body)
		}
	})

	t.Run("not your learner: refused", func(t *testing.T) {
		if rec := join(pool, friend, token, payer.learners[0]); errorCode(t, rec) != "unknown_learner" {
			t.Errorf("%d %s", rec.Code, rec.Body)
		}
	})

	t.Run("the friend joins; the link is used up", func(t *testing.T) {
		if rec := join(pool, friend, token, friend.learners[0]); rec.Code != 200 {
			t.Fatalf("%d %s", rec.Code, rec.Body)
		}
		if n := count(t, pool, `SELECT count(*) FROM booking_attendee WHERE id = $1 AND status = 'joined'
			AND learner_id = $2 AND invite_token IS NULL`, seats[0], friend.learners[0]); n != 1 {
			t.Error("seat not filled as expected")
		}
		other := newStudent(t, pool, 0, false)
		if rec := join(pool, other, token, other.learners[0]); errorCode(t, rec) != "seat_link_invalid" {
			t.Errorf("the same link again: %d %s", rec.Code, rec.Body)
		}
		if rec := seatReq(pool, seatLink(pool, "https://hablemos.live"), payer.cookie, b.ID, seats[0]); errorCode(t, rec) != "seat_not_open" {
			t.Errorf("a link for a filled seat: %d %s", rec.Code, rec.Body)
		}
		var list []myBookingDTO
		json.Unmarshal(call(requireSignIn(pool, listMyBookings(pool)), "GET", "/bookings", payer.cookie).Body.Bytes(), &list)
		if len(list) != 1 || list[0].OpenSeats != 1 {
			t.Errorf("payer's view: %+v", list)
		}
	})

	t.Run("the same person can't take two seats", func(t *testing.T) {
		second := seatLinkToken(t, pool, payer, b.ID, seats[1])
		if rec := join(pool, friend, second, friend.learners[0]); errorCode(t, rec) != "already_in_class" {
			t.Errorf("%d %s", rec.Code, rec.Body)
		}
	})

	t.Run("the friend leaves: the seat opens again, no money moves", func(t *testing.T) {
		before := balance(t, pool, payer.accountID)
		if rec := leave(pool, friend, seats[0]); rec.Code != 200 {
			t.Fatalf("%d %s", rec.Code, rec.Body)
		}
		if n := count(t, pool, `SELECT count(*) FROM booking_attendee WHERE id = $1 AND status = 'invited' AND learner_id IS NULL`, seats[0]); n != 1 ||
			balance(t, pool, payer.accountID) != before {
			t.Error("not reopened, or money moved")
		}
		if rec := leave(pool, friend, seats[0]); rec.Code != 404 {
			t.Errorf("leaving twice: %d", rec.Code)
		}
	})

	t.Run("a cancelled booking: links dead, joining refused", func(t *testing.T) {
		t2 := seatLinkToken(t, pool, payer, b.ID, seats[0])
		if rec := cancel(pool, payer.cookie, b.ID); rec.Code != 200 {
			t.Fatalf("cancel: %d", rec.Code)
		}
		if rec := preview(pool, t2); rec.Code != 404 {
			t.Errorf("preview: %d", rec.Code)
		}
		if rec := join(pool, friend, t2, friend.learners[0]); errorCode(t, rec) != "not_changeable" {
			t.Errorf("join: %d %s", rec.Code, rec.Body)
		}
		_ = ctx
	})
}

// Two friends use the same link at once. The test plays the first: it locks
// the seat and fills it, holding the transaction open while the second joins.
// With the seat's FOR UPDATE, the second waits, then sees the seat taken.
// Without it, it reads "invited" and overwrites the first friend.
func TestJoinSeatRace(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	payer, b, seats := privateWithOpenSeats(t, pool, 20)
	token := seatLinkToken(t, pool, payer, b.ID, seats[0])
	first, second := newStudent(t, pool, 0, false), newStudent(t, pool, 0, false)

	gate, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer gate.Rollback(ctx)
	if _, err := gate.Exec(ctx, `
		UPDATE booking_attendee SET status = 'joined', learner_id = $2, invite_token = NULL WHERE id = $1`,
		seats[0], first.learners[0]); err != nil {
		t.Fatal(err)
	}
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- join(pool, second, token, second.learners[0]) }()
	waitForLockWaiters(t, pool, 1)
	if err := gate.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	rec := <-done
	if n := count(t, pool, `SELECT count(*) FROM booking_attendee WHERE id = $1 AND learner_id = $2`, seats[0], first.learners[0]); n != 1 {
		t.Errorf("the first friend lost the seat (%d %s)", rec.Code, rec.Body)
	}
	if rec.Code == 200 {
		t.Errorf("the second friend joined a taken seat")
	}
}

// A friend joins while the payer cancels the booking. The test plays the
// cancellation: it locks the booking and cancels it, holding the transaction
// open while the friend joins. With the booking's shared lock, the join waits,
// then sees it cancelled. Without it, the friend ends up in a cancelled booking.
func TestJoinWhileCancelled(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	payer, b, seats := privateWithOpenSeats(t, pool, 21)
	token := seatLinkToken(t, pool, payer, b.ID, seats[0])
	friend := newStudent(t, pool, 0, false)

	gate, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer gate.Rollback(ctx)
	if _, err := gate.Exec(ctx, `UPDATE booking SET status = 'cancelled', cancelled_at = now() WHERE id = $1`, b.ID); err != nil {
		t.Fatal(err)
	}
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- join(pool, friend, token, friend.learners[0]) }()
	deadline := time.Now().Add(3 * time.Second)
	for len(done) == 0 && count(t, pool, `SELECT count(*) FROM pg_stat_activity WHERE datname = current_database() AND wait_event_type = 'Lock'`) == 0 &&
		time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if err := gate.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	rec := <-done
	if n := count(t, pool, `SELECT count(*) FROM booking_attendee WHERE id = $1 AND status = 'joined'`, seats[0]); n != 0 {
		t.Errorf("the friend joined a cancelled booking (%d %s)", rec.Code, rec.Body)
	}
}
