package main

// POST /webhooks/stripe: Stripe tells us a Checkout payment happened (or
// didn't). This is the only code that adds purchased credit, and anyone on the
// internet can send a request here, so nothing in the body is trusted until
// Stripe's signature checks out.

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const webhookTolerance = 5 * time.Minute // older (or future) signatures are refused: no replays

// verifyStripeSignature checks the Stripe-Signature header ("t=...,v1=...,v1=...")
// against the raw body: HMAC-SHA256(secret, t + "." + body) must equal one of
// the v1 values, and t must be within webhookTolerance of now.
func verifyStripeSignature(header string, body []byte, secret string, now time.Time) error {
	var timestamp string
	var signatures []string
	for _, part := range strings.Split(header, ",") {
		k, v, _ := strings.Cut(strings.TrimSpace(part), "=")
		switch k {
		case "t":
			timestamp = v
		case "v1":
			signatures = append(signatures, v)
		}
	}
	unix, err := strconv.ParseInt(timestamp, 10, 64)
	if err != nil || len(signatures) == 0 {
		return errors.New("signature header missing or malformed")
	}
	if age := now.Sub(time.Unix(unix, 0)); age > webhookTolerance || age < -webhookTolerance {
		return errors.New("signature timestamp too old or in the future")
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(timestamp + "."))
	mac.Write(body)
	expected := mac.Sum(nil)
	for _, s := range signatures {
		got, err := hex.DecodeString(s)
		// hmac.Equal compares in constant time: how long it takes doesn't reveal
		// how much of the signature was right.
		if err == nil && hmac.Equal(got, expected) {
			return nil
		}
	}
	return errors.New("signature doesn't match")
}

// stripeEvent is the part of a Stripe event we read. For Checkout events,
// data.object is the Checkout session.
type stripeEvent struct {
	ID   string `json:"id"`
	Type string `json:"type"`
	Data struct {
		Object struct {
			ID                string `json:"id"`                  // cs_...
			ClientReferenceID string `json:"client_reference_id"` // our payment ID
			PaymentStatus     string `json:"payment_status"`      // "paid", "unpaid", "no_payment_required"
			AmountTotal       int    `json:"amount_total"`        // cents
			Currency          string `json:"currency"`            // "usd"
		} `json:"object"`
	} `json:"data"`
}

// stripeWebhook handles POST /webhooks/stripe. Answers: 400 for anything not
// signed by Stripe; 200 once the event is handled or deliberately ignored (so
// Stripe stops resending); 500 if our database failed (so Stripe resends).
func stripeWebhook(pool *pgxpool.Pool, secret string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if secret == "" {
			writeErrorCode(w, r, http.StatusServiceUnavailable, "payments_unavailable", "webhooks aren't configured")
			return
		}
		// The raw bytes, exactly as sent: the signature covers them, not a re-encoding.
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 256<<10))
		if err != nil {
			writeError(w, r, http.StatusBadRequest, "could not read the body")
			return
		}
		if err := verifyStripeSignature(r.Header.Get("Stripe-Signature"), body, secret, time.Now()); err != nil {
			logFrom(r).Warn("stripe webhook: rejected", "reason", err.Error())
			writeError(w, r, http.StatusBadRequest, "invalid signature")
			return
		}
		var ev stripeEvent
		if err := json.Unmarshal(body, &ev); err != nil {
			writeError(w, r, http.StatusBadRequest, "body is not a Stripe event")
			return
		}
		session := ev.Data.Object
		log := logFrom(r).With("stripe_event", ev.ID, "event_type", ev.Type, "payment_id", session.ClientReferenceID)

		var outcome string
		switch {
		case ev.Type == "checkout.session.completed" && session.PaymentStatus == "paid",
			ev.Type == "checkout.session.async_payment_succeeded":
			outcome, err = creditPayment(r.Context(), pool, session.ClientReferenceID, session.ID, session.AmountTotal, session.Currency)
		case ev.Type == "checkout.session.async_payment_failed", ev.Type == "checkout.session.expired":
			outcome, err = failPayment(r.Context(), pool, session.ClientReferenceID)
		default:
			outcome = "ignored" // other events, and "completed" while a delayed payment is still pending
		}
		if err != nil {
			log.Error("stripe webhook: handling failed", "err", err)
			writeError(w, r, http.StatusInternalServerError, "could not handle the event") // Stripe will resend
			return
		}
		log.Info("stripe webhook handled", "outcome", outcome)
		writeJSON(w, http.StatusOK, map[string]string{"outcome": outcome})
	}
}

// creditPayment marks the payment succeeded and adds the credit it bought, in
// one transaction. Safe to call any number of times for the same payment: the
// row lock makes simultaneous deliveries queue, and every one after the first
// finds it already succeeded. Outcomes: "credited", "already_credited",
// "unknown_payment", "amount_mismatch" (logged; nothing credited).
func creditPayment(ctx context.Context, pool *pgxpool.Pool, paymentID, sessionID string, amountCents int, currency string) (outcome string, err error) {
	if !isUUID(paymentID) {
		return "unknown_payment", nil
	}
	err = pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		var p struct {
			accountID, status, currency, providerRef string
			amountCents, bonusCents                  int
		}
		err := tx.QueryRow(ctx, `
			SELECT account_id::text, status, amount_cents, bonus_cents, currency, COALESCE(provider_ref, '')
			FROM payment WHERE id = $1 AND provider = 'stripe' FOR UPDATE`, paymentID).
			Scan(&p.accountID, &p.status, &p.amountCents, &p.bonusCents, &p.currency, &p.providerRef)
		if errors.Is(err, pgx.ErrNoRows) {
			outcome = "unknown_payment" // e.g. a staging event sent to production: nothing to do
			return nil
		}
		if err != nil {
			return err
		}
		if p.status == "succeeded" {
			outcome = "already_credited"
			return nil
		}
		// Trust, but verify: Stripe's amount, currency and session must be ours.
		if amountCents != p.amountCents || !strings.EqualFold(currency, p.currency) ||
			(p.providerRef != "" && p.providerRef != sessionID) {
			outcome = "amount_mismatch"
			slog.Error("stripe webhook: payment doesn't match, not credited", "payment_id", paymentID,
				"want_cents", p.amountCents, "got_cents", amountCents, "want_currency", p.currency, "got_currency", currency,
				"want_session", p.providerRef, "got_session", sessionID)
			return nil
		}

		if _, err := tx.Exec(ctx, `
			UPDATE payment SET status = 'succeeded', provider_ref = $2 WHERE id = $1`, paymentID, sessionID); err != nil {
			return err
		}
		// What was paid, and the bonus promised when the purchase started (the snapshot).
		if _, err := tx.Exec(ctx, `
			INSERT INTO credit_ledger (id, account_id, entry_type, amount_cents, currency, payment_id)
			VALUES ($1, $2, 'purchase', $3, $4, $5)`, newUUIDv7(), p.accountID, p.amountCents, p.currency, paymentID); err != nil {
			return err
		}
		if p.bonusCents > 0 {
			if _, err := tx.Exec(ctx, `
				INSERT INTO credit_ledger (id, account_id, entry_type, amount_cents, currency, payment_id)
				VALUES ($1, $2, 'bonus', $3, $4, $5)`, newUUIDv7(), p.accountID, p.bonusCents, p.currency, paymentID); err != nil {
				return err
			}
		}
		payload, _ := json.Marshal(map[string]any{
			"payment_id": paymentID, "account_id": p.accountID,
			"amount_cents": p.amountCents, "bonus_cents": p.bonusCents, "currency": p.currency,
		})
		if _, err := tx.Exec(ctx, `
			INSERT INTO outbox_event (id, aggregate_type, aggregate_id, event_type, payload)
			VALUES ($1, 'payment', $2, 'payment_succeeded', $3)`, newUUIDv7(), paymentID, payload); err != nil {
			return err
		}
		outcome = "credited"
		return nil
	})
	return outcome, err
}

// failPayment marks a still-pending payment failed (checkout expired, or a
// delayed payment didn't go through). A succeeded payment is never touched.
func failPayment(ctx context.Context, pool *pgxpool.Pool, paymentID string) (string, error) {
	if !isUUID(paymentID) {
		return "unknown_payment", nil
	}
	tag, err := pool.Exec(ctx, `
		UPDATE payment SET status = 'failed' WHERE id = $1 AND provider = 'stripe' AND status = 'pending'`, paymentID)
	if err != nil {
		return "", err
	}
	if tag.RowsAffected() == 0 {
		return "not_pending", nil
	}
	return "failed", nil
}
