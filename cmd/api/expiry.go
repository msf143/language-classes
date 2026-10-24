package main

// Credit expiry (decided 2026-10-24): when an account has had no credit
// activity for credit_policy.expire_after_months, its remaining balance
// expires. "Activity" is any ledger entry other than an expiry: buying credit,
// booking, a refund. Two of the worker's clock-driven checks live here: the
// reminder some days before, and the expiry itself. Like the others in
// checks.go, both are safe to run every minute: each acts only on what's due
// and not yet done.

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// creditStanding is the query both checks start from: per account and
// currency, the balance, when the credit was last active, and when it expires.
// All derived from the ledger and the policy row; nothing is stored.
const creditStanding = `
	WITH standing AS (
		SELECT l.account_id, l.currency, SUM(l.amount_cents)::int AS balance_cents,
		       MAX(l.created_at) FILTER (WHERE l.entry_type <> 'expiry') AS last_activity
		FROM credit_ledger l
		GROUP BY l.account_id, l.currency
	)
	SELECT s.account_id::text, s.currency, s.balance_cents, s.last_activity,
	       s.last_activity + make_interval(months => p.expire_after_months) AS expires_at,
	       p.warn_days_before
	FROM standing s CROSS JOIN credit_policy p
	WHERE s.balance_cents > 0`

type standing struct {
	accountID, currency     string
	balanceCents, warnDays  int
	lastActivity, expiresAt time.Time
}

func scanStanding(row pgx.CollectableRow) (standing, error) {
	var s standing
	err := row.Scan(&s.accountID, &s.currency, &s.balanceCents, &s.lastActivity, &s.expiresAt, &s.warnDays)
	return s, err
}

// queueExpiryReminders queues one "credit_expiry_reminder" per account whose
// credit expires within warn_days_before days. Once per stretch of inactivity:
// the dedupe_key holds the last-activity time, so the unique index ignores
// repeats, while an account that becomes active and later goes quiet again
// gets a new reminder (new last activity, new key).
func queueExpiryReminders(ctx context.Context, pool *pgxpool.Pool) (queued int, err error) {
	rows, err := pool.Query(ctx, creditStanding+`
		  AND p.warn_days_before > 0
		  AND s.last_activity + make_interval(months => p.expire_after_months) > now()
		  AND s.last_activity + make_interval(months => p.expire_after_months, days => -p.warn_days_before) <= now()`)
	if err != nil {
		return 0, err
	}
	due, err := pgx.CollectRows(rows, scanStanding)
	if err != nil {
		return 0, err
	}
	for _, s := range due {
		payload, _ := json.Marshal(map[string]any{
			"account_id": s.accountID, "balance_cents": s.balanceCents, "currency": s.currency,
			"last_activity": s.lastActivity, "expires_at": s.expiresAt,
		})
		key := "expiry-reminder:" + s.accountID + ":" + s.currency + ":" + s.lastActivity.UTC().Format(time.RFC3339Nano)
		tag, err := pool.Exec(ctx, `
			INSERT INTO outbox_event (id, aggregate_type, aggregate_id, event_type, payload, dedupe_key)
			VALUES ($1, 'account', $2, 'credit_expiry_reminder', $3, $4)
			ON CONFLICT (dedupe_key) WHERE dedupe_key IS NOT NULL DO NOTHING`,
			newUUIDv7(), s.accountID, payload, key)
		if err != nil {
			return queued, err
		}
		queued += int(tag.RowsAffected())
	}
	return queued, nil
}

// expireCredit expires the balance of every account whose credit is past its
// expiry. One transaction per account (expireAccount).
func expireCredit(ctx context.Context, pool *pgxpool.Pool) (expired int, err error) {
	rows, err := pool.Query(ctx, creditStanding+`
		  AND s.last_activity + make_interval(months => p.expire_after_months) <= now()`)
	if err != nil {
		return 0, err
	}
	due, err := pgx.CollectRows(rows, scanStanding)
	if err != nil {
		return 0, err
	}
	for _, s := range due {
		done, err := expireAccount(ctx, pool, s.accountID, s.currency)
		if err != nil {
			return expired, err
		}
		if done {
			expired++
		}
	}
	return expired, nil
}

// expireAccount writes one 'expiry' entry for the account's whole balance, if
// it's still due. It locks the account row first, the same first lock booking
// and cancelling take: a booking at this moment either commits first (new
// activity, so nothing expires) or waits and then finds a zero balance. The
// balance and last activity are read again after the lock, because what the
// list query saw may have changed. Only the account is locked, so this can't
// deadlock with booking (account, then session) or the group check (session).
func expireAccount(ctx context.Context, pool *pgxpool.Pool, accountID, currency string) (expired bool, err error) {
	err = pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT 1 FROM account WHERE id = $1 FOR UPDATE`, accountID); err != nil {
			return err
		}
		var s standing
		err := tx.QueryRow(ctx, creditStanding+`
			  AND s.account_id = $1 AND s.currency = $2
			  AND s.last_activity + make_interval(months => p.expire_after_months) <= now()`,
			accountID, currency).
			Scan(&s.accountID, &s.currency, &s.balanceCents, &s.lastActivity, &s.expiresAt, &s.warnDays)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil // no longer due: spent, topped up or already expired meanwhile
		}
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO credit_ledger (id, account_id, entry_type, amount_cents, currency, note)
			VALUES ($1, $2, 'expiry', $3, $4, $5)`,
			newUUIDv7(), accountID, -s.balanceCents, currency,
			"no credit activity since "+s.lastActivity.UTC().Format("2006-01-02")); err != nil {
			return err
		}
		payload, _ := json.Marshal(map[string]any{
			"account_id": accountID, "expired_cents": s.balanceCents, "currency": currency,
			"last_activity": s.lastActivity,
		})
		if _, err := tx.Exec(ctx, `
			INSERT INTO outbox_event (id, aggregate_type, aggregate_id, event_type, payload)
			VALUES ($1, 'account', $2, 'credit_expired', $3)`, newUUIDv7(), accountID, payload); err != nil {
			return err
		}
		expired = true
		return nil
	})
	return expired, err
}
