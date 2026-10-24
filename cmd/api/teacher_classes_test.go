package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// signedInTeacherWithPrices: a signed-in teacher (Bogotá) with a group price
// for 30 and 60 minutes, every party size; 45 has none.
func signedInTeacherWithPrices(t *testing.T, pool *pgxpool.Pool) (*http.Cookie, string) {
	t.Helper()
	cookie, teacherID := newSignedInTeacher(t, pool)
	for _, minutes := range []int{30, 60} {
		for size := 1; size <= maxPartySize; size++ {
			if _, err := pool.Exec(context.Background(), `
				INSERT INTO price_tier (id, teacher_id, kind, duration_minutes, party_size, price_per_person_cents)
				VALUES ($1, $2, 'group', $3, $4, 1500)`, newUUIDv7(), teacherID, minutes, size); err != nil {
				t.Fatal(err)
			}
		}
	}
	return cookie, teacherID
}

// classBody: a valid new group class `ahead` from now (Bogotá wall-clock time), with changes.
func classBody(ahead time.Duration, changes map[string]any) string {
	bogota, _ := time.LoadLocation("America/Bogota")
	at := time.Now().In(bogota).Add(ahead).Truncate(time.Hour)
	body := map[string]any{
		"title": "Conversación", "target_language": "es", "audience": "adults",
		"date": at.Format("2006-01-02"), "from": at.Format("15:04"),
		"duration_minutes": 60, "min_seats": 2, "max_seats": 8,
	}
	for k, v := range changes {
		body[k] = v
	}
	b, _ := json.Marshal(body)
	return string(b)
}

func createClass(t *testing.T, pool *pgxpool.Pool, cookie *http.Cookie, body string) *httptest.ResponseRecorder {
	t.Helper()
	rec := send(pool, createGroupClass(pool), "POST", "/teacher/sessions", body, "", cookie)
	if rec.Code == http.StatusCreated {
		var c struct{ ID string }
		json.Unmarshal(rec.Body.Bytes(), &c)
		cancelWhenDone(t, pool, c.ID)
	}
	return rec
}

func TestCreateGroupClass(t *testing.T) {
	pool := testPool(t)
	cookie, teacherID := signedInTeacherWithPrices(t, pool)

	rec := createClass(t, pool, cookie, classBody(96*time.Hour, nil))
	if rec.Code != http.StatusCreated {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	var created struct {
		ID       string    `json:"id"`
		StartsAt time.Time `json:"starts_at"`
	}
	json.Unmarshal(rec.Body.Bytes(), &created)
	if n := count(t, pool, `
		SELECT count(*) FROM session WHERE id = $1 AND teacher_id = $2 AND kind = 'group' AND status = 'scheduled'
		  AND ends_at = starts_at + interval '60 minutes' AND occupied = tstzrange(starts_at, ends_at)
		  AND min_seats = 2 AND max_seats = 8 AND title = 'Conversación'
		  AND extract(hour FROM starts_at AT TIME ZONE 'America/Bogota') = $3`,
		created.ID, teacherID, created.StartsAt.In(mustZone("America/Bogota")).Hour()); n != 1 {
		t.Errorf("the session row isn't as asked")
	}

	tests := []struct {
		name       string
		body       string
		wantStatus int
		wantCode   string
	}{
		{"no title", classBody(96*time.Hour, map[string]any{"title": "  "}), 400, "invalid_class"},
		{"unknown language", classBody(96*time.Hour, map[string]any{"target_language": "fr"}), 400, "invalid_class"},
		{"unknown audience", classBody(96*time.Hour, map[string]any{"audience": "teens"}), 400, "invalid_class"},
		{"not a date", classBody(96*time.Hour, map[string]any{"date": "next Tuesday"}), 400, "invalid_class"},
		{"90 minutes", classBody(96*time.Hour, map[string]any{"duration_minutes": 90}), 400, "invalid_class"},
		{"minimum of 1", classBody(96*time.Hour, map[string]any{"min_seats": 1}), 400, "invalid_class"},
		{"maximum below minimum", classBody(96*time.Hour, map[string]any{"min_seats": 4, "max_seats": 3}), 400, "invalid_class"},
		{"maximum over 30", classBody(96*time.Hour, map[string]any{"max_seats": 31}), 400, "invalid_class"},
		{"an unknown field", strings.Replace(classBody(96*time.Hour, nil), `"title"`, `"titel"`, 1), 400, ""},
		{"only 30 hours ahead", classBody(30*time.Hour, nil), 409, "too_soon"},
		{"no group price for 45 minutes", classBody(120*time.Hour, map[string]any{"duration_minutes": 45}), 409, "no_price"},
		{"overlaps his first class", classBody(96*time.Hour, map[string]any{"duration_minutes": 30}), 409, "class_overlap"},
	}
	for _, tt := range tests {
		rec := createClass(t, pool, cookie, tt.body)
		if rec.Code != tt.wantStatus || (tt.wantCode != "" && errorCode(t, rec) != tt.wantCode) {
			t.Errorf("%s: %d %s, want %d %s", tt.name, rec.Code, rec.Body, tt.wantStatus, tt.wantCode)
		}
	}
	t.Run("no max_seats: no cap", func(t *testing.T) {
		rec := createClass(t, pool, cookie, classBody(144*time.Hour, map[string]any{"max_seats": nil}))
		if rec.Code != http.StatusCreated {
			t.Fatalf("%d %s", rec.Code, rec.Body)
		}
	})
}

func mustZone(name string) *time.Location {
	loc, _ := time.LoadLocation(name)
	return loc
}

// The teacher cancels a class two students booked: both get everything back,
// each gets the "teacher had to cancel" email; cancelling again does nothing
// more; another teacher's class is a 404; a class that has started can't be.
func TestTeacherCancelsClass(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	cookie, teacherID := signedInTeacherWithPrices(t, pool)
	class := newSession(t, pool, teacherID, sessionOpts{slot: 30})
	ana, ben := newStudent(t, pool, 5000, false), newStudent(t, pool, 5000, false)
	a, _ := bookOK(t, pool, ana, class), bookOK(t, pool, ben, class)
	cancel := func(c *http.Cookie, id string) *httptest.ResponseRecorder {
		return send(pool, cancelTeacherSession(pool), "POST", "/teacher/sessions/"+id+"/cancel", "", id, c)
	}

	rec := cancel(cookie, class)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"bookings_refunded":2`) {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	for _, s := range []student{ana, ben} {
		if b := balance(t, pool, s.accountID); b != 5000 {
			t.Errorf("balance %d after the teacher cancelled, want 5000 (everything back)", b)
		}
	}
	if n := count(t, pool, `SELECT count(*) FROM session WHERE id = $1 AND status = 'cancelled' AND cancel_reason = 'teacher_cancelled'`, class); n != 1 {
		t.Error("session not cancelled as teacher_cancelled")
	}
	if n := count(t, pool, `SELECT count(*) FROM outbox_event WHERE event_type = 'booking_cancelled' AND payload->>'session_id' = $1 AND payload->>'cancelled_by' = 'teacher'`, class); n != 2 {
		t.Errorf("%d cancellation events, want 2", n)
	}

	t.Run("the email says the teacher cancelled", func(t *testing.T) {
		m := &recordingMailer{}
		if err := notifyHandler(pool, m.send, "https://hablemos.live")(ctx, eventFor(t, pool, a.ID, "booking_cancelled")); err != nil {
			t.Fatal(err)
		}
		if len(m.sent) != 1 || !strings.Contains(m.sent[0].Text, "The teacher had to cancel this class") ||
			!strings.Contains(m.sent[0].Text, "El profesor tuvo que cancelar") {
			t.Errorf("%d emails: %+v", len(m.sent), m.sent)
		}
	})

	t.Run("again: nothing more", func(t *testing.T) {
		if rec := cancel(cookie, class); rec.Code != 200 || balance(t, pool, ana.accountID) != 5000 {
			t.Errorf("%d %s, balance %d", rec.Code, rec.Body, balance(t, pool, ana.accountID))
		}
	})

	t.Run("not his class: 404", func(t *testing.T) {
		other := newSession(t, pool, newTeacher(t, pool), sessionOpts{slot: 31})
		if rec := cancel(cookie, other); rec.Code != 404 {
			t.Errorf("%d %s", rec.Code, rec.Body)
		}
	})

	t.Run("started: 409", func(t *testing.T) {
		started := newSession(t, pool, teacherID, sessionOpts{slot: -1})
		if rec := cancel(cookie, started); rec.Code != 409 || errorCode(t, rec) != "class_started" {
			t.Errorf("%d %s", rec.Code, rec.Body)
		}
	})
}

// A student books while the teacher cancels. The test holds the class's row
// locked (a gate), starts both, waits until both are blocked, then lets go.
// Either the booking comes first and is refunded by the cancel, or the cancel
// comes first and the booking is refused. Never a paid booking in a cancelled
// class.
func TestTeacherCancelRace(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	cookie, teacherID := signedInTeacherWithPrices(t, pool)
	class := newSession(t, pool, teacherID, sessionOpts{slot: 32})
	s := newStudent(t, pool, 5000, false)

	gate, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer gate.Rollback(ctx)
	if _, err := gate.Exec(ctx, `SELECT 1 FROM session WHERE id = $1 FOR UPDATE`, class); err != nil {
		t.Fatal(err)
	}
	booked := make(chan *httptest.ResponseRecorder, 1)
	cancelled := make(chan *httptest.ResponseRecorder, 1)
	go func() { booked <- book(pool, s.cookie, newUUIDv7(), class, s.learners...) }()
	go func() {
		cancelled <- send(pool, cancelTeacherSession(pool), "POST", "/teacher/sessions/"+class+"/cancel", "", class, cookie)
	}()
	waitForLockWaiters(t, pool, 2)
	if err := gate.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	b, c := <-booked, <-cancelled

	if c.Code != 200 {
		t.Fatalf("cancel: %d %s", c.Code, c.Body)
	}
	if b.Code != http.StatusCreated && errorCode(t, b) != "session_not_open" {
		t.Errorf("booking: %d %s, want 201 or session_not_open", b.Code, b.Body)
	}
	if n := count(t, pool, `SELECT count(*) FROM booking WHERE session_id = $1 AND status = 'confirmed'`, class); n != 0 {
		t.Errorf("%d confirmed bookings in a cancelled class", n)
	}
	if got := balance(t, pool, s.accountID); got != 5000 {
		t.Errorf("balance %d, want 5000: the student paid for a cancelled class", got)
	}
	t.Logf("order: booking %d, cancel refunded %s", b.Code, c.Body)
}

// The worker cancels a class for being under the minimum while the teacher
// cancels it too. The test plays the worker: it locks the class and cancels it
// (under_minimum), holding its transaction open while the teacher's cancel
// starts. With the teacher's FOR UPDATE, his cancel waits, then sees the class
// already cancelled and leaves it as it is: the reason stays "under_minimum",
// which is what the students' emails will say. Without it, his cancel reads
// "scheduled" too early and overwrites the reason.
func TestTeacherCancelAfterWorker(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	cookie, teacherID := signedInTeacherWithPrices(t, pool)
	class := newSession(t, pool, teacherID, sessionOpts{slot: 33})

	gate, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer gate.Rollback(ctx)
	if _, err := gate.Exec(ctx, `SELECT 1 FROM session WHERE id = $1 FOR UPDATE`, class); err != nil {
		t.Fatal(err)
	}
	if _, err := cancelWithRefunds(ctx, gate, class, "under_minimum", "system"); err != nil {
		t.Fatal(err)
	}
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		done <- send(pool, cancelTeacherSession(pool), "POST", "/teacher/sessions/"+class+"/cancel", "", class, cookie)
	}()
	waitForLockWaiters(t, pool, 1)
	if err := gate.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if rec := <-done; rec.Code != 200 {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	var reason string
	pool.QueryRow(ctx, `SELECT cancel_reason FROM session WHERE id = $1`, class).Scan(&reason)
	if reason != "under_minimum" {
		t.Errorf("cancel_reason %q, want under_minimum (the worker's decision came first)", reason)
	}
}
