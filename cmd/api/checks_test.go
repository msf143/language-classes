package main

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

// These tests run in a private schema (migratedSchemaPool): the checks look at
// every class and booking in the database, and the dev database's own seed
// classes mustn't be decided (or cancelled) by a test.

func TestCheckGroupMinimums(t *testing.T) {
	pool := migratedSchemaPool(t, 8)
	ctx := context.Background()
	teacher := newTeacher(t, pool)
	status := func(id string) (s string, reason *string) {
		t.Helper()
		if err := pool.QueryRow(ctx, `SELECT status, cancel_reason FROM session WHERE id = $1`, id).Scan(&s, &reason); err != nil {
			t.Fatal(err)
		}
		return s, reason
	}

	// Within 24 hours (10 h away): one with a single seat booked, one with two.
	under := newSession(t, pool, teacher, sessionOpts{slot: 5})
	enough := newSession(t, pool, teacher, sessionOpts{slot: 6})
	later := newSession(t, pool, teacher, sessionOpts{slot: 20}) // 40 h away: not decided yet
	lonely := newStudent(t, pool, 5000, false)
	lonelyBooking := bookOK(t, pool, lonely, under)
	bookOK(t, pool, newStudent(t, pool, 5000, false), enough)
	bookOK(t, pool, newStudent(t, pool, 5000, false), enough)

	confirmed, cancelled, err := checkGroupMinimums(ctx, pool)
	if err != nil || confirmed != 1 || cancelled != 1 {
		t.Fatalf("confirmed %d, cancelled %d, err %v; want 1 and 1", confirmed, cancelled, err)
	}
	if s, reason := status(under); s != "cancelled" || reason == nil || *reason != "under_minimum" {
		t.Errorf("under-minimum class: %s %v, want cancelled under_minimum", s, reason)
	}
	if s, _ := status(enough); s != "confirmed" {
		t.Errorf("class with 2 seats: %s, want confirmed", s)
	}
	if s, _ := status(later); s != "scheduled" {
		t.Errorf("class 40 h away: %s, want still scheduled", s)
	}

	// The lonely student got everything back, and a cancellation event is queued
	// (the worker turns it into the "didn't reach the minimum" email).
	if got := balance(t, pool, lonely.accountID); got != 5000 {
		t.Errorf("balance %d, want 5000 (full refund)", got)
	}
	var payload []byte
	if err := pool.QueryRow(ctx, `SELECT payload FROM outbox_event WHERE aggregate_id = $1 AND event_type = 'booking_cancelled'`,
		lonelyBooking.ID).Scan(&payload); err != nil {
		t.Fatalf("no booking_cancelled event: %v", err)
	}
	var p struct {
		CancelledBy string `json:"cancelled_by"`
		Reason      string `json:"reason"`
	}
	json.Unmarshal(payload, &p)
	if p.CancelledBy != "system" || p.Reason != "under_minimum" {
		t.Errorf("event payload %s; want cancelled_by system, reason under_minimum", payload)
	}

	// The confirmed class still takes bookings; the cancelled one doesn't.
	more := newStudent(t, pool, 5000, false)
	if rec := book(pool, more.cookie, "k", enough, more.learners...); rec.Code != 201 {
		t.Errorf("booking the confirmed class: %d %s, want 201", rec.Code, rec.Body)
	}
	late := newStudent(t, pool, 5000, false)
	if rec := book(pool, late.cookie, "k", under, late.learners...); errorCode(t, rec) != "session_not_open" {
		t.Errorf("booking the cancelled class: %d %s, want session_not_open", rec.Code, rec.Body)
	}

	// Running again changes nothing.
	if c, x, err := checkGroupMinimums(ctx, pool); err != nil || c+x != 0 {
		t.Errorf("second run: confirmed %d, cancelled %d, err %v; want nothing", c, x, err)
	}
}

func TestQueueReminders(t *testing.T) {
	pool := migratedSchemaPool(t, 8)
	ctx := context.Background()
	teacher := newTeacher(t, pool)
	soon := newSession(t, pool, teacher, sessionOpts{maxSeats: seats(8), slot: 5}) // 10 h away

	early := newStudent(t, pool, 5000, false)       // booked "two days ago"
	justNow := newStudent(t, pool, 5000, false)     // booked within 24 h of the class
	changedMind := newStudent(t, pool, 5000, false) // booked early, then cancelled
	earlyB := bookOK(t, pool, early, soon)
	bookOK(t, pool, justNow, soon)
	changedB := bookOK(t, pool, changedMind, soon)
	pool.Exec(ctx, `UPDATE booking SET created_at = now() - interval '2 days' WHERE id = ANY($1::uuid[])`,
		[]string{earlyB.ID, changedB.ID})
	if rec := cancel(pool, changedMind.cookie, changedB.ID); rec.Code != 200 {
		t.Fatalf("cancel: %d %s", rec.Code, rec.Body)
	}

	queued, err := queueReminders(ctx, pool)
	if err != nil || queued != 1 {
		t.Fatalf("queued %d, err %v; want exactly 1 (the early booking)", queued, err)
	}
	if n := count(t, pool, `SELECT count(*) FROM outbox_event WHERE event_type = 'booking_reminder' AND aggregate_id = $1`, earlyB.ID); n != 1 {
		t.Errorf("%d reminders for the early booking, want 1", n)
	}
	if again, _ := queueReminders(ctx, pool); again != 0 {
		t.Errorf("second run queued %d more, want 0 (dedupe_key)", again)
	}

	// The email: to the payer only; skipped if the booking was cancelled meanwhile.
	ev := eventFor(t, pool, earlyB.ID, "booking_reminder")
	m := &recordingMailer{}
	if err := notifyHandler(pool, m.send, "https://hablemos.live")(ctx, ev); err != nil {
		t.Fatal(err)
	}
	if len(m.sent) != 1 || !strings.HasPrefix(m.sent[0].Subject, "Reminder: ") || m.sent[0].IdempotencyKey != ev.ID+":payer" {
		t.Errorf("reminder emails %+v", m.sent)
	}
	cancel(pool, early.cookie, earlyB.ID)
	var sk *skipError
	if err := notifyHandler(pool, m.send, "https://hablemos.live")(ctx, ev); !errors.As(err, &sk) {
		t.Errorf("reminder for a cancelled booking: %v, want a skip", err)
	}
}

func TestPurgeExpired(t *testing.T) {
	pool := migratedSchemaPool(t, 4)
	ctx := context.Background()
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatal(err)
		}
	}
	token := func(expiredAgo time.Duration) {
		exec(`INSERT INTO login_token (id, email, token_hash, created_at, expires_at)
		      VALUES ($1, 'a@example.com', $2, now() - $3::int * interval '1 second' - interval '15 minutes',
		              now() - $3::int * interval '1 second')`,
			newUUIDv7(), hashSecret(newSecret()), int(expiredAgo.Seconds()))
	}
	token(2 * 24 * time.Hour) // gone
	token(time.Hour)          // kept: rate limits and support may still want it
	account, _ := newAccount(t, pool)
	webSession := func(expiredAgo time.Duration) {
		exec(`INSERT INTO web_session (id, account_id, token_hash, created_at, expires_at)
		      VALUES ($1, $2, $3, now() - $4::int * interval '1 second' - interval '30 days',
		              now() - $4::int * interval '1 second')`,
			newUUIDv7(), account, hashSecret(newSecret()), int(expiredAgo.Seconds()))
	}
	webSession(8 * 24 * time.Hour) // gone
	webSession(24 * time.Hour)     // kept for now

	tokens, sessions, err := purgeExpired(ctx, pool)
	if err != nil || tokens != 1 || sessions != 1 {
		t.Errorf("purged %d tokens and %d sessions (err %v); want 1 and 1", tokens, sessions, err)
	}
}
