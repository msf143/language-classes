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
	"fmt"
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

// ---------- the emails (the worker's notifyHandler sends these) ----------

// notifyExpiryReminder: "your credit expires on …". The reminder was queued
// up to a minute earlier, so the standing is read again now: if the student
// booked or bought since (a new expiry date) or has nothing left, the email
// isn't needed and is skipped.
func notifyExpiryReminder(ctx context.Context, pool *pgxpool.Pool, send mailer, siteURL string, ev outboxEvent) error {
	var queued struct {
		Currency  string    `json:"currency"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	if err := json.Unmarshal(ev.Payload, &queued); err != nil {
		return skip("bad payload: " + err.Error())
	}
	var s standing
	var name, to, tz string
	var months int
	err := pool.QueryRow(ctx, creditStanding+`
		  AND s.account_id = $1 AND s.currency = $2`, ev.AggregateID, queued.Currency).
		Scan(&s.accountID, &s.currency, &s.balanceCents, &s.lastActivity, &s.expiresAt, &s.warnDays)
	if errors.Is(err, pgx.ErrNoRows) {
		return skip("no credit left")
	}
	if err != nil {
		return err
	}
	if !s.expiresAt.Equal(queued.ExpiresAt) {
		return skip("credit used since the reminder was queued")
	}
	if err := pool.QueryRow(ctx, `
		SELECT a.display_name, a.email, a.timezone, p.expire_after_months
		FROM account a CROSS JOIN credit_policy p WHERE a.id = $1`, ev.AggregateID).
		Scan(&name, &to, &tz, &months); err != nil {
		return err
	}
	loc := zone(tz)
	return sendLogged(ctx, send, ev, "account", email{
		From: classesEmailFrom, To: to, IdempotencyKey: ev.ID + ":account",
		Subject: "Your credit expires on " + dateEN(s.expiresAt, loc) + " · Tu crédito vence el " + dateES(s.expiresAt, loc),
		Text: fmt.Sprintf(`Hi %[1]s,

  You have %[2]s of credit with Hablemos.
  Credit expires after %[3]d months without any activity. Yours was last
  used on %[4]s, so it expires on %[5]s.

To keep it, book any class or buy credit before then: %[6]s

—

Hola, %[1]s:

  Tienes %[7]s de crédito en Hablemos.
  El crédito vence tras %[3]d meses sin actividad. El tuyo se usó por
  última vez el %[8]s, así que vence el %[9]s.

Para conservarlo, reserva cualquier clase o compra crédito antes de esa fecha: %[6]s
`, name, moneyEN(s.balanceCents, s.currency), months, dateEN(s.lastActivity, loc), dateEN(s.expiresAt, loc), siteURL,
			moneyES(s.balanceCents, s.currency), dateES(s.lastActivity, loc), dateES(s.expiresAt, loc)),
	})
}

// notifyCreditExpired: "your credit expired". The amount comes from the event,
// written in the same transaction as the expiry entry, so it's what expired.
func notifyCreditExpired(ctx context.Context, pool *pgxpool.Pool, send mailer, siteURL string, ev outboxEvent) error {
	var p struct {
		ExpiredCents int       `json:"expired_cents"`
		Currency     string    `json:"currency"`
		LastActivity time.Time `json:"last_activity"`
	}
	if err := json.Unmarshal(ev.Payload, &p); err != nil {
		return skip("bad payload: " + err.Error())
	}
	var name, to, tz string
	var months int
	if err := pool.QueryRow(ctx, `
		SELECT a.display_name, a.email, a.timezone, pol.expire_after_months
		FROM account a CROSS JOIN credit_policy pol WHERE a.id = $1`, ev.AggregateID).
		Scan(&name, &to, &tz, &months); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return skip("account not found")
		}
		return err
	}
	loc := zone(tz)
	return sendLogged(ctx, send, ev, "account", email{
		From: classesEmailFrom, To: to, IdempotencyKey: ev.ID + ":account",
		Subject: "Your credit of " + moneyEN(p.ExpiredCents, p.Currency) + " has expired · Tu crédito de " +
			moneyES(p.ExpiredCents, p.Currency) + " venció",
		Text: fmt.Sprintf(`Hi %[1]s,

  Your credit of %[2]s has expired: it had not been used for %[3]d months
  (last activity on %[4]s).

You're welcome back any time: %[5]s

—

Hola, %[1]s:

  Tu crédito de %[6]s venció: no se había usado en %[3]d meses
  (última actividad el %[7]s).

Te esperamos cuando quieras: %[5]s
`, name, moneyEN(p.ExpiredCents, p.Currency), months, dateEN(p.LastActivity, loc), siteURL,
			moneyES(p.ExpiredCents, p.Currency), dateES(p.LastActivity, loc)),
	})
}
