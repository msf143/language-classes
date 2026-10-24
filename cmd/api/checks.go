package main

// The worker's time-based checks: things that happen because the clock moved,
// not because someone clicked. Each run of `api worker` does these first, then
// drains the outbox, so the emails they queue go out in the same run. All of
// them are safe to run every minute: each one only acts on what's still to do.

import (
	"context"
	"encoding/json"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	groupDecisionNotice = 24 * time.Hour // a group class is confirmed or cancelled this long before it starts
	groupMinStudents    = 2              // the least any group class may require (constraint session_group_min_seats)
)

// checkGroupMinimums decides every group class that starts within the next 24
// hours and is still undecided ("scheduled"): enough seats → "confirmed" (it
// runs even if people drop later); too few → cancelled ("under_minimum") with
// a full refund for every booking. One transaction per class.
func checkGroupMinimums(ctx context.Context, pool *pgxpool.Pool) (confirmed, cancelled int, err error) {
	rows, err := pool.Query(ctx, `
		SELECT id::text FROM session
		WHERE kind = 'group' AND status = 'scheduled'
		  AND starts_at > now() AND starts_at <= now() + $1::int * interval '1 second'
		ORDER BY starts_at`, int(groupDecisionNotice.Seconds()))
	if err != nil {
		return 0, 0, err
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return 0, 0, err
	}
	for _, id := range ids {
		outcome, err := decideGroupSession(ctx, pool, id)
		if err != nil {
			return confirmed, cancelled, err
		}
		switch outcome {
		case "confirmed":
			confirmed++
		case "cancelled":
			cancelled++
		}
	}
	return confirmed, cancelled, nil
}

// decideGroupSession confirms or cancels one class. It locks the session row
// first: a booking for this class arriving at the same moment waits, then sees
// the decision (and can still book a confirmed class, but not a cancelled one).
//
// Locks: the session, then its bookings, never an account. Booking takes the
// account, then the session; since this never waits for an account, the two
// can't deadlock. (Refund entries don't need the account lock: that lock
// guards balance checks, and a refund doesn't check a balance.)
func decideGroupSession(ctx context.Context, pool *pgxpool.Pool, sessionID string) (outcome string, err error) {
	err = pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		var status string
		var minSeats, taken int
		err := tx.QueryRow(ctx, `
			SELECT s.status, s.min_seats,
			       (SELECT COALESCE(SUM(b.party_size), 0)::int FROM booking b
			        WHERE b.session_id = s.id AND b.status = 'confirmed')
			FROM session s WHERE s.id = $1 FOR UPDATE OF s`, sessionID).Scan(&status, &minSeats, &taken)
		if err != nil {
			return err
		}
		if status != "scheduled" {
			return nil // decided meanwhile (another worker, or the teacher)
		}
		if taken >= minSeats {
			outcome = "confirmed"
			_, err = tx.Exec(ctx, `UPDATE session SET status = 'confirmed' WHERE id = $1`, sessionID)
			return err
		}

		outcome = "cancelled"
		if _, err := tx.Exec(ctx, `
			UPDATE session SET status = 'cancelled', cancel_reason = 'under_minimum' WHERE id = $1`, sessionID); err != nil {
			return err
		}
		// Every confirmed booking: cancelled with a full refund.
		rows, err := tx.Query(ctx, `
			UPDATE booking SET status = 'cancelled', cancelled_at = now(), refund_cents = total_cents
			WHERE session_id = $1 AND status = 'confirmed'
			RETURNING id::text, payer_account_id::text, total_cents, currency`, sessionID)
		if err != nil {
			return err
		}
		type refund struct {
			bookingID, payerID, currency string
			cents                        int
		}
		refunds, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (refund, error) {
			var r refund
			err := row.Scan(&r.bookingID, &r.payerID, &r.cents, &r.currency)
			return r, err
		})
		if err != nil {
			return err
		}
		for _, r := range refunds {
			if r.cents > 0 {
				if _, err := tx.Exec(ctx, `
					INSERT INTO credit_ledger (id, account_id, entry_type, amount_cents, currency, booking_id, note)
					VALUES ($1, $2, 'refund', $3, $4, $5, 'class cancelled: under the minimum')`,
					newUUIDv7(), r.payerID, r.cents, r.currency, r.bookingID); err != nil {
					return err
				}
			}
			payload, _ := json.Marshal(map[string]any{
				"booking_id": r.bookingID, "session_id": sessionID, "payer_account_id": r.payerID,
				"total_cents": r.cents, "refund_cents": r.cents, "currency": r.currency,
				"cancelled_by": "system", "reason": "under_minimum",
			})
			if _, err := tx.Exec(ctx, `
				INSERT INTO outbox_event (id, aggregate_type, aggregate_id, event_type, payload)
				VALUES ($1, 'booking', $2, 'booking_cancelled', $3)`, newUUIDv7(), r.bookingID, payload); err != nil {
				return err
			}
		}
		return nil
	})
	return outcome, err
}

// queueReminders queues one "booking_reminder" event per confirmed booking whose
// class starts within the next 24 hours, if it was booked more than 24 hours
// before the class (someone who just booked doesn't need reminding). The
// dedupe_key makes "once" a database guarantee: the unique index turns a second
// insert for the same booking into a no-op, however often this runs.
func queueReminders(ctx context.Context, pool *pgxpool.Pool) (queued int, err error) {
	rows, err := pool.Query(ctx, `
		SELECT b.id::text FROM booking b JOIN session s ON s.id = b.session_id
		WHERE b.status = 'confirmed' AND s.status IN ('scheduled', 'confirmed')
		  AND s.starts_at > now() AND s.starts_at <= now() + interval '24 hours'
		  AND b.created_at < s.starts_at - interval '24 hours'
		  AND NOT EXISTS (SELECT 1 FROM outbox_event e WHERE e.dedupe_key = 'reminder:' || b.id::text)`)
	if err != nil {
		return 0, err
	}
	bookingIDs, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return 0, err
	}
	for _, id := range bookingIDs {
		tag, err := pool.Exec(ctx, `
			INSERT INTO outbox_event (id, aggregate_type, aggregate_id, event_type, payload, dedupe_key)
			VALUES ($1, 'booking', $2, 'booking_reminder', '{}', $3)
			ON CONFLICT (dedupe_key) WHERE dedupe_key IS NOT NULL DO NOTHING`, newUUIDv7(), id, "reminder:"+id)
		if err != nil {
			return queued, err
		}
		queued += int(tag.RowsAffected())
	}
	return queued, nil
}

// purgeExpired deletes sign-in leftovers nobody can use any more: login links
// expired over a day ago, and browser sessions expired or signed out over a
// week ago. These are security artefacts, not business records, so they are
// deleted rather than kept with a status.
func purgeExpired(ctx context.Context, pool *pgxpool.Pool) (tokens, sessions int64, err error) {
	tag, err := pool.Exec(ctx, `DELETE FROM login_token WHERE expires_at < now() - interval '1 day'`)
	if err != nil {
		return 0, 0, err
	}
	tokens = tag.RowsAffected()
	tag, err = pool.Exec(ctx, `
		DELETE FROM web_session
		WHERE expires_at < now() - interval '7 days' OR revoked_at < now() - interval '7 days'`)
	if err != nil {
		return tokens, 0, err
	}
	return tokens, tag.RowsAffected(), nil
}
