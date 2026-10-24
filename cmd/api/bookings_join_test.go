package main

import (
	"context"
	"encoding/json"
	"errors"
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

// The friend's side: the class in their My bookings as a guest (no money
// shown), and the reminder and cancellation emails reach them too, with their
// own people's names and times in their own zone.
func TestGuestSide(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	payer, b, seats := privateWithOpenSeats(t, pool, 19)
	friend := newStudent(t, pool, 0, false)
	pool.Exec(ctx, `UPDATE account SET display_name = 'Ana', timezone = 'America/Bogota' WHERE id = $1`, friend.accountID)
	pool.Exec(ctx, `UPDATE learner SET display_name = 'Tomás' WHERE id = $1`, friend.learners[0])
	if rec := join(pool, friend, seatLinkToken(t, pool, payer, b.ID, seats[0]), friend.learners[0]); rec.Code != 200 {
		t.Fatalf("join: %d %s", rec.Code, rec.Body)
	}

	t.Run("My bookings: guest for the friend, payer for the payer", func(t *testing.T) {
		var mine []myBookingDTO
		json.Unmarshal(call(requireSignIn(pool, listMyBookings(pool)), "GET", "/bookings", friend.cookie).Body.Bytes(), &mine)
		if len(mine) != 1 || mine[0].Role != "guest" || mine[0].PaidBy != "Test" || mine[0].TotalCents != 0 ||
			len(mine[0].MySeats) != 1 || mine[0].MySeats[0] != seats[0] {
			t.Errorf("friend: %+v", mine)
		}
		var theirs []myBookingDTO
		json.Unmarshal(call(requireSignIn(pool, listMyBookings(pool)), "GET", "/bookings", payer.cookie).Body.Bytes(), &theirs)
		if len(theirs) != 1 || theirs[0].Role != "payer" || theirs[0].TotalCents != 5100 {
			t.Errorf("payer: %+v", theirs)
		}
	})

	handle := func(eventType string) []email {
		ev := outboxEvent{ID: newUUIDv7(), AggregateType: "booking", AggregateID: b.ID, EventType: eventType, Payload: []byte(`{}`)}
		m := &recordingMailer{}
		if err := notifyHandler(pool, m.send, "https://hablemos.live")(ctx, ev); err != nil {
			t.Fatal(err)
		}
		return m.sent
	}

	t.Run("the reminder reaches the friend, with their own people", func(t *testing.T) {
		sent := handle("booking_reminder")
		if len(sent) != 2 {
			t.Fatalf("%d emails, want payer + friend", len(sent))
		}
		g := sent[1]
		if !strings.HasPrefix(g.Text, "Hi Ana,") || !strings.Contains(g.Text, "For: Tomás") || !strings.Contains(g.Text, "America/Bogota") {
			t.Errorf("friend's reminder:\n%s", g.Text)
		}
	})

	t.Run("a cancellation reaches the friend, without the refund line", func(t *testing.T) {
		if rec := cancel(pool, payer.cookie, b.ID); rec.Code != 200 {
			t.Fatalf("cancel: %d", rec.Code)
		}
		sent := handle("booking_cancelled") // payer, teacher (private class), friend
		if len(sent) != 3 {
			t.Fatalf("%d emails, want payer + teacher + friend", len(sent))
		}
		g := sent[2]
		if g.Subject != "Class cancelled: Private class (Spanish) · Clase cancelada: Clase privada (español)" ||
			!strings.Contains(g.Text, "Booked by Test") || !strings.Contains(g.Text, "Nothing was charged to you") ||
			strings.Contains(g.Text, "went back to your credit") {
			t.Errorf("friend's cancellation: %s\n%s", g.Subject, g.Text)
		}
	})
}

// latestEvent: the newest outbox event of that type for the booking.
func latestEvent(t *testing.T, pool *pgxpool.Pool, bookingID, eventType string) outboxEvent {
	t.Helper()
	var ev outboxEvent
	if err := pool.QueryRow(context.Background(), `
		SELECT id::text, aggregate_type, aggregate_id::text, event_type, payload, created_at, attempts
		FROM outbox_event WHERE aggregate_id = $1 AND event_type = $2 ORDER BY created_at DESC, id DESC LIMIT 1`, bookingID, eventType).
		Scan(&ev.ID, &ev.AggregateType, &ev.AggregateID, &ev.EventType, &ev.Payload, &ev.CreatedAt, &ev.Attempts); err != nil {
		t.Fatalf("no %s event: %v", eventType, err)
	}
	return ev
}

// The seat emails, through a whole friendship: join, a seat added, leave,
// join again, removed by the payer.
func TestSeatEmails(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	teacher, day := privateTeacher(t, pool)
	payer := newStudent(t, pool, 10000, false)
	b := decodeBooking(t, bookSeats(pool, payer.cookie, newUUIDv7(), teacher, day.Add(19*time.Hour), 2, payer.learners...)) // 2 × $20
	cancelWhenDone(t, pool, b.SessionID)
	friend := newStudent(t, pool, 0, false)
	pool.Exec(ctx, `UPDATE account SET display_name = 'Ana' WHERE id = $1`, friend.accountID)
	pool.Exec(ctx, `UPDATE learner SET display_name = 'Tomás' WHERE id = $1`, friend.learners[0])
	handle := func(eventType string) []email {
		t.Helper()
		m := &recordingMailer{}
		if err := notifyHandler(pool, m.send, "https://hablemos.live")(ctx, latestEvent(t, pool, b.ID, eventType)); err != nil {
			t.Fatal(err)
		}
		return m.sent
	}
	contains := func(e email, parts ...string) bool {
		for _, p := range parts {
			if !strings.Contains(e.Subject+"\n"+e.Text, p) {
				return false
			}
		}
		return true
	}

	seat := openSeat(t, pool, b.ID)
	if rec := join(pool, friend, seatLinkToken(t, pool, payer, b.ID, seat), friend.learners[0]); rec.Code != 200 {
		t.Fatalf("join: %d", rec.Code)
	}
	if sent := handle("seat_joined"); len(sent) != 2 || !contains(sent[0], "A friend joined", "Ana joined your class (Tomás)") ||
		!contains(sent[1], "You're in", "For: Tomás", "Booked by Test (nothing to pay)") {
		t.Errorf("joined: %+v", sent)
	}

	decodeSeat(t, seatReq(pool, addSeat(pool), payer.cookie, b.ID, "")) // 2 × $20 → 3 × $17: $11 more
	if sent := handle("seats_changed"); len(sent) != 1 || !contains(sent[0], "now has 3 seat(s)", "$11.00 paid from your credit") {
		t.Errorf("seat added: %+v", sent)
	}

	if rec := leave(pool, friend, seat); rec.Code != 200 {
		t.Fatalf("leave: %d", rec.Code)
	}
	if sent := handle("seat_left"); len(sent) != 1 || !contains(sent[0], "A friend left", "Ana left your class (Tomás)", "The seat is open again") {
		t.Errorf("left: %+v", sent)
	}
	// The "joined" event, handled now that they've left: skipped, nothing sent.
	m := &recordingMailer{}
	err := notifyHandler(pool, m.send, "https://hablemos.live")(ctx, latestEvent(t, pool, b.ID, "seat_joined"))
	var sk *skipError
	if !errors.As(err, &sk) || len(m.sent) != 0 {
		t.Errorf("a stale 'joined': err %v, sent %+v; want a skip and nothing sent", err, m.sent)
	}

	if rec := join(pool, friend, seatLinkToken(t, pool, payer, b.ID, seat), friend.learners[0]); rec.Code != 200 {
		t.Fatalf("join again: %d", rec.Code)
	}
	decodeSeat(t, seatReq(pool, removeSeat(pool), payer.cookie, b.ID, seat)) // 3 → 2 seats: $11 back, friend removed
	if sent := handle("seats_changed"); len(sent) != 2 || !contains(sent[0], "now has 2 seat(s)", "$11.00 went back to your credit") ||
		!contains(sent[1], "Removed from a class", "Test removed Tomás from this class", "Nothing was charged to you") {
		t.Errorf("friend's seat removed: %+v", sent)
	}
}
