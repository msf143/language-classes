package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestWhenAndMoney(t *testing.T) {
	ny, _ := time.LoadLocation("America/New_York")
	start := time.Date(2026, 10, 28, 0, 0, 0, 0, time.UTC) // 8 PM the evening before, in New York
	end := start.Add(time.Hour)
	tests := []struct{ got, want string }{
		{whenEN(start, end, ny), "Tuesday, October 27, 8:00 PM – 9:00 PM (America/New_York)"},
		{whenES(start, end, ny), "martes 27 de octubre, 20:00 – 21:00 (America/New_York)"},
		{shortWhen(start, ny), "Tue Oct 27 20:00"},
		{moneyEN(2400, "USD"), "$24.00"},
		{moneyES(2400, "USD"), "24,00 US$"},
		{moneyEN(5, "USD"), "$0.05"},
	}
	for _, tt := range tests {
		if tt.got != tt.want {
			t.Errorf("got %q, want %q", tt.got, tt.want)
		}
	}
}

// recordingMailer remembers what would have been sent; fail makes it error.
type recordingMailer struct {
	sent []email
	fail bool
}

func (m *recordingMailer) send(_ context.Context, e email) (string, error) {
	if m.fail {
		return "", errors.New("mail server down")
	}
	m.sent = append(m.sent, e)
	return "id", nil
}

// eventFor finds the outbox event a booking wrote (in the dev database).
func eventFor(t *testing.T, pool *pgxpool.Pool, bookingID, eventType string) outboxEvent {
	t.Helper()
	var ev outboxEvent
	err := pool.QueryRow(context.Background(), `
		SELECT id::text, aggregate_type, aggregate_id::text, event_type, payload, created_at, attempts
		FROM outbox_event WHERE aggregate_id = $1 AND event_type = $2`, bookingID, eventType).
		Scan(&ev.ID, &ev.AggregateType, &ev.AggregateID, &ev.EventType, &ev.Payload, &ev.CreatedAt, &ev.Attempts)
	if err != nil {
		t.Fatal(err)
	}
	return ev
}

func accountEmail(t *testing.T, pool *pgxpool.Pool, accountID string) string {
	t.Helper()
	var e string
	if err := pool.QueryRow(context.Background(), `SELECT email FROM account WHERE id = $1`, accountID).Scan(&e); err != nil {
		t.Fatal(err)
	}
	return e
}

func sessionTimes(t *testing.T, pool *pgxpool.Pool, sessionID string) (time.Time, time.Time) {
	t.Helper()
	var s, e time.Time
	if err := pool.QueryRow(context.Background(), `SELECT starts_at, ends_at FROM session WHERE id = $1`, sessionID).Scan(&s, &e); err != nil {
		t.Fatal(err)
	}
	return s, e
}

func TestNotifyBooking(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	ny, _ := time.LoadLocation("America/New_York")
	const site = "https://hablemos.live"

	// A student in New York books a 60-minute group class.
	s := newStudent(t, pool, 5000, false)
	pool.Exec(ctx, `UPDATE account SET timezone = 'America/New_York' WHERE id = $1`, s.accountID)
	group := newSession(t, pool, newTeacher(t, pool), sessionOpts{slot: 30})
	b := bookOK(t, pool, s, group)
	start, end := sessionTimes(t, pool, group)

	t.Run("booked: one email to the payer, times in their zone, both languages", func(t *testing.T) {
		ev := eventFor(t, pool, b.ID, "booking_created")
		m := &recordingMailer{}
		if err := notifyHandler(pool, m.send, site)(ctx, ev); err != nil {
			t.Fatal(err)
		}
		if len(m.sent) != 1 {
			t.Fatalf("%d emails, want 1 (group class: no teacher copy)", len(m.sent))
		}
		e := m.sent[0]
		for _, want := range []string{whenEN(start, end, ny), whenES(start, end, ny), "Paid: $15.00", "Pagado: 15,00 US$", site} {
			if !strings.Contains(e.Text, want) {
				t.Errorf("text lacks %q:\n%s", want, e.Text)
			}
		}
		if e.To != accountEmail(t, pool, s.accountID) || !strings.HasPrefix(e.Subject, "You're booked: ") ||
			e.IdempotencyKey != ev.ID+":payer" || e.From != classesEmailFrom {
			t.Errorf("email %+v", e)
		}
	})

	t.Run("cancelled: the refund, and a booked event that comes later is skipped", func(t *testing.T) {
		if rec := cancel(pool, s.cookie, b.ID); rec.Code != http.StatusOK {
			t.Fatalf("cancel: %d %s", rec.Code, rec.Body)
		}
		m := &recordingMailer{}
		if err := notifyHandler(pool, m.send, site)(ctx, eventFor(t, pool, b.ID, "booking_cancelled")); err != nil {
			t.Fatal(err)
		}
		if len(m.sent) != 1 || !strings.Contains(m.sent[0].Text, "$15.00 went back to your credit") ||
			!strings.Contains(m.sent[0].Text, "Se devolvieron 15,00 US$") {
			t.Errorf("cancellation emails %+v", m.sent)
		}
		var sk *skipError
		if err := notifyHandler(pool, m.send, site)(ctx, eventFor(t, pool, b.ID, "booking_created")); !errors.As(err, &sk) {
			t.Errorf("booked email for a cancelled booking: %v, want a skip", err)
		}
	})

	t.Run("private class: the teacher gets a copy, in his own zone", func(t *testing.T) {
		teacher, day := privateTeacher(t, pool)
		pool.Exec(ctx, `UPDATE account SET timezone = 'America/Bogota'
			WHERE id = (SELECT account_id FROM teacher WHERE id = $1)`, teacher)
		p := newStudent(t, pool, 5000, false)
		rec := bookPrivate(pool, p.cookie, "k", teacher, day.Add(20*time.Hour), 60, p.learners...)
		if rec.Code != http.StatusCreated {
			t.Fatalf("private booking: %d %s", rec.Code, rec.Body)
		}
		pb := decodeBooking(t, rec)
		cancelWhenDone(t, pool, pb.SessionID)

		m := &recordingMailer{}
		ev := eventFor(t, pool, pb.ID, "booking_created")
		if err := notifyHandler(pool, m.send, site)(ctx, ev); err != nil {
			t.Fatal(err)
		}
		if len(m.sent) != 2 {
			t.Fatalf("%d emails, want 2 (payer and teacher)", len(m.sent))
		}
		bogota, _ := time.LoadLocation("America/Bogota")
		toTeacher := m.sent[1]
		if !strings.HasPrefix(toTeacher.Subject, "New private class") || toTeacher.IdempotencyKey != ev.ID+":teacher" ||
			!strings.Contains(toTeacher.Text, whenEN(day.Add(20*time.Hour), day.Add(21*time.Hour), bogota)) ||
			!strings.Contains(toTeacher.Text, "Booked by: ") {
			t.Errorf("teacher email %+v", toTeacher)
		}
		if !strings.Contains(m.sent[0].Text, "Private class (Spanish)") || !strings.Contains(m.sent[0].Text, "Clase privada (español)") {
			t.Errorf("payer email doesn't name the private class:\n%s", m.sent[0].Text)
		}
	})

	t.Run("a mail failure is returned (so the worker retries); unknown events and bookings are skipped", func(t *testing.T) {
		ev := eventFor(t, pool, b.ID, "booking_cancelled")
		if err := notifyHandler(pool, (&recordingMailer{fail: true}).send, site)(ctx, ev); err == nil {
			t.Error("mail failure not reported")
		}
		var sk *skipError
		if err := notifyHandler(pool, (&recordingMailer{}).send, site)(ctx, outboxEvent{EventType: "session_rescheduled"}); !errors.As(err, &sk) {
			t.Errorf("unknown event type: %v, want a skip", err)
		}
		gone := outboxEvent{EventType: "booking_created", AggregateID: newUUIDv7()}
		if err := notifyHandler(pool, (&recordingMailer{}).send, site)(ctx, gone); !errors.As(err, &sk) {
			t.Errorf("missing booking: %v, want a skip", err)
		}
	})
}

// The Resend mailer passes the idempotency key on as a header.
func TestResendMailerIdempotencyKey(t *testing.T) {
	var gotKey string
	fake := httptestServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotKey = r.Header.Get("Idempotency-Key")
		w.Write([]byte(`{"id":"x"}`))
	})
	send := resendMailer("re_test", fake+"/emails", "")
	if _, err := send(context.Background(), email{From: classesEmailFrom, To: "a@example.com", Subject: "s", Text: "t", IdempotencyKey: "event-1:payer"}); err != nil {
		t.Fatal(err)
	}
	if gotKey != "event-1:payer" {
		t.Errorf("Idempotency-Key header %q, want event-1:payer", gotKey)
	}
}

// httptestServer starts a fake HTTP server for the test and returns its URL.
func httptestServer(t *testing.T, h http.HandlerFunc) string {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv.URL
}
