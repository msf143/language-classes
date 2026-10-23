package main

// The worker: reads outbox_event, the table every booking and cancellation
// writes an event to in the same transaction as the data (the outbox pattern:
// change-data capture without a CDC tool). For each due event it calls a
// handler (sending an email, say) and records the outcome.
//
// Delivery is at-least-once: if the worker crashes after the handler ran but
// before its commit, the event is handed out again. Handlers must therefore be
// idempotent (the email sender passes the event ID as an idempotency key).

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	maxAttempts    = 10             // after this many failures an event is left alone (see last_error)
	staleAfter     = 48 * time.Hour // older events aren't acted on any more (e.g. a week-old "you're booked")
	firstRetry     = time.Minute    // retry delays double from here: 1, 2, 4 ... minutes
	maxRetryDelay  = time.Hour      // and never wait longer than this
	maxErrorLength = 500            // keep last_error short
)

// outboxEvent is one row of outbox_event, as a handler sees it.
type outboxEvent struct {
	ID            string
	AggregateType string // "booking", "session", ...
	AggregateID   string
	EventType     string // "booking_created", "booking_cancelled", ...
	Payload       []byte // JSON
	CreatedAt     time.Time
	Attempts      int // failed attempts so far
}

// eventHandler does the work for one event. Returning nil marks it processed;
// an error schedules a retry; a *skipError marks it processed without retrying
// (with the reason in last_error).
type eventHandler func(ctx context.Context, ev outboxEvent) error

// skipError means "nothing to do for this event, and retrying won't change
// that" (an event type nobody handles, a booking that no longer exists...).
type skipError struct{ reason string }

func (e *skipError) Error() string { return "skipped: " + e.reason }

func skip(reason string) error { return &skipError{reason} }

// drainStats counts what one drainOutbox call did.
type drainStats struct {
	Processed, Failed, Skipped int
}

// drainOutbox handles due events, one per transaction, until none are due, the
// context ends, or limit events have been handled. Several workers can run at
// once: FOR UPDATE SKIP LOCKED gives each one a different event.
func drainOutbox(ctx context.Context, pool *pgxpool.Pool, handle eventHandler, limit int) (drainStats, error) {
	var stats drainStats
	for stats.Processed+stats.Failed+stats.Skipped < limit {
		if ctx.Err() != nil {
			return stats, nil // out of time: the rest waits for the next run
		}
		found, err := handleNextEvent(ctx, pool, handle, &stats)
		if err != nil {
			return stats, err
		}
		if !found {
			return stats, nil // nothing due
		}
	}
	return stats, nil
}

// handleNextEvent claims the next due event, runs the handler, records the
// outcome, and commits. It reports whether there was an event at all.
func handleNextEvent(ctx context.Context, pool *pgxpool.Pool, handle eventHandler, stats *drainStats) (found bool, err error) {
	err = pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		// Claim one due event. SKIP LOCKED: if another worker holds a row, don't
		// wait for it; take the next one. That's what lets workers run side by side.
		var ev outboxEvent
		var stale bool
		err := tx.QueryRow(ctx, `
			SELECT id::text, aggregate_type, aggregate_id::text, event_type, payload, created_at, attempts,
			       created_at < now() - $2::int * interval '1 second'
			FROM outbox_event
			WHERE processed_at IS NULL AND available_at <= now() AND attempts < $1
			ORDER BY available_at, created_at
			LIMIT 1
			FOR UPDATE SKIP LOCKED`, maxAttempts, int(staleAfter.Seconds())).
			Scan(&ev.ID, &ev.AggregateType, &ev.AggregateID, &ev.EventType, &ev.Payload, &ev.CreatedAt, &ev.Attempts, &stale)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		found = true

		var outcome error
		if stale {
			outcome = skip("stale")
		} else {
			outcome = handle(ctx, ev)
		}

		var sk *skipError
		switch {
		case outcome == nil:
			stats.Processed++
			_, err = tx.Exec(ctx, `UPDATE outbox_event SET processed_at = now(), last_error = NULL WHERE id = $1`, ev.ID)
		case errors.As(outcome, &sk):
			stats.Skipped++
			slog.Info("outbox event skipped", "event_id", ev.ID, "event_type", ev.EventType, "reason", sk.reason)
			_, err = tx.Exec(ctx, `UPDATE outbox_event SET processed_at = now(), last_error = $2 WHERE id = $1`,
				ev.ID, truncate(outcome.Error()))
		default:
			stats.Failed++
			attempts := ev.Attempts + 1
			delay := retryDelay(attempts)
			slog.Warn("outbox event failed", "event_id", ev.ID, "event_type", ev.EventType,
				"attempt", attempts, "retry_in", delay.String(), "err", outcome,
				"gave_up", attempts >= maxAttempts)
			_, err = tx.Exec(ctx, `
				UPDATE outbox_event
				SET attempts = $2, last_error = $3, available_at = now() + $4::int * interval '1 second'
				WHERE id = $1`, ev.ID, attempts, truncate(outcome.Error()), int(delay.Seconds()))
		}
		return err
	})
	return found, err
}

// retryDelay after the nth failed attempt: 1, 2, 4, 8 ... minutes, at most an hour.
func retryDelay(attempt int) time.Duration {
	d := firstRetry
	for i := 1; i < attempt && d < maxRetryDelay; i++ {
		d *= 2
	}
	return min(d, maxRetryDelay)
}

func truncate(s string) string {
	if len(s) > maxErrorLength {
		return s[:maxErrorLength]
	}
	return s
}

// runWorker is `api worker`: one pass over the outbox, then exit. The Cloud
// Run job runs it every minute; it stops itself after 50 seconds so one run is
// always done before the next starts (overlapping would be safe, just wasteful).
func runWorker(ctx context.Context, pool *pgxpool.Pool, appEnv, resendKey, siteURL string) error {
	var send mailer
	switch {
	case appEnv == "dev":
		send = logMailer // never email real people from a dev setup
	case resendKey != "":
		prefix := ""
		if appEnv == "staging" {
			prefix = "[TEST] "
		}
		send = resendMailer(resendKey, resendEndpoint, prefix)
	default:
		// Better to fail loudly than to mark events processed without sending.
		return errors.New("no mailer: set RESEND_API_KEY (or APP_ENV=dev to log emails instead)")
	}

	ctx, cancel := context.WithTimeout(ctx, 50*time.Second)
	defer cancel()

	// The clock-driven checks first, so the emails they queue go out in this
	// same run. A failing check is logged and the run goes on: a problem with
	// reminders mustn't hold up booking confirmations.
	if confirmed, cancelled, err := checkGroupMinimums(ctx, pool); err != nil {
		slog.Error("group minimum check failed", "err", err)
	} else if confirmed+cancelled > 0 {
		slog.Info("group classes decided", "confirmed", confirmed, "cancelled", cancelled)
	}
	if queued, err := queueReminders(ctx, pool); err != nil {
		slog.Error("queueing reminders failed", "err", err)
	} else if queued > 0 {
		slog.Info("reminders queued", "count", queued)
	}
	if tokens, sessions, err := purgeExpired(ctx, pool); err != nil {
		slog.Error("purge failed", "err", err)
	} else if tokens+sessions > 0 {
		slog.Info("expired sign-ins purged", "login_tokens", tokens, "web_sessions", sessions)
	}

	stats, err := drainOutbox(ctx, pool, notifyHandler(pool, send, siteURL), 500)
	slog.Info("worker run done", "processed", stats.Processed, "failed", stats.Failed, "skipped", stats.Skipped)
	return err
}
