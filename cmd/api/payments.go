package main

// Buying credit: the packs on offer, and (from step 6b) paying for one with
// Stripe Checkout.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// creditPackDTO is one pack on offer: pay price_cents, get credit_cents.
type creditPackDTO struct {
	ID          string `json:"id"`
	PriceCents  int    `json:"price_cents"`
	BonusCents  int    `json:"bonus_cents"`
	CreditCents int    `json:"credit_cents"` // price + bonus: what lands in the balance
	Currency    string `json:"currency"`
}

// listCreditPacks handles GET /credit-packs (public): the active packs, in
// display order.
func listCreditPacks(pool *pgxpool.Pool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		rows, err := pool.Query(r.Context(), `
			SELECT id::text, price_cents, bonus_cents, price_cents + bonus_cents, currency
			FROM credit_pack WHERE status = 'active'
			ORDER BY sort_order, price_cents`)
		if err != nil {
			logFrom(r).Error("credit packs: query failed", "err", err)
			writeError(w, r, http.StatusInternalServerError, "could not load the credit packs")
			return
		}
		packs, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (creditPackDTO, error) {
			var p creditPackDTO
			err := row.Scan(&p.ID, &p.PriceCents, &p.BonusCents, &p.CreditCents, &p.Currency)
			return p, err
		})
		if err != nil {
			logFrom(r).Error("credit packs: scan failed", "err", err)
			writeError(w, r, http.StatusInternalServerError, "could not load the credit packs")
			return
		}
		if packs == nil {
			packs = []creditPackDTO{} // [] rather than null
		}
		writeJSON(w, http.StatusOK, packs)
	}
}

// startCheckout handles POST /payments/checkout {"pack_id": "..."} (wrapped in
// requireSignIn, with an Idempotency-Key header): it records a pending payment
// and returns the URL of a Stripe Checkout page for it. The student pays on
// Stripe's page; the credit arrives with Stripe's webhook (POST /webhooks/stripe).
//
// The same key again returns the same payment and the same Stripe page, so a
// double click or a retry never opens two checkouts.
func startCheckout(pool *pgxpool.Pool, stripe *stripeClient, siteURL string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if stripe == nil {
			writeErrorCode(w, r, http.StatusServiceUnavailable, "payments_unavailable", "buying credit isn't available yet")
			return
		}
		me, _ := accountFrom(r)
		key := r.Header.Get("Idempotency-Key")
		if key == "" || len(key) > 100 {
			writeError(w, r, http.StatusBadRequest, "send an Idempotency-Key header (a unique string per purchase attempt, up to 100 characters)")
			return
		}
		var body struct {
			PackID string `json:"pack_id"`
		}
		r.Body = http.MaxBytesReader(w, r.Body, 1<<10)
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || !isUUID(body.PackID) {
			writeError(w, r, http.StatusBadRequest, `body must be JSON like {"pack_id": "..."}`)
			return
		}

		p, err := recordPendingPayment(r.Context(), pool, me.ID, key, strings.ToLower(body.PackID))
		var be *bookingError
		switch {
		case errors.As(err, &be):
			writeErrorCode(w, r, be.status, be.code, be.msg)
			return
		case err != nil:
			logFrom(r).Error("checkout: recording the payment failed", "err", err)
			writeError(w, r, http.StatusInternalServerError, "could not start the payment")
			return
		}

		// Outside any transaction: never hold the database while waiting on Stripe.
		credit := p.priceCents + p.bonusCents
		sessionID, pageURL, err := stripe.createCheckoutSession(r.Context(), checkoutRequest{
			PaymentID:   p.id,
			AmountCents: p.priceCents,
			Currency:    p.currency,
			ProductName: "Hablemos credit: " + moneyEN(credit, p.currency) + " · Crédito Hablemos: " + moneyES(credit, p.currency),
			Email:       me.Email,
			SuccessURL:  siteURL + "/?paid=" + p.id,
			CancelURL:   siteURL + "/?paid=cancelled",
		})
		if err != nil {
			logFrom(r).Error("checkout: Stripe refused or unreachable", "err", err, "payment_id", p.id)
			writeErrorCode(w, r, http.StatusBadGateway, "payment_provider_error", "the payment service didn't answer; please try again")
			return
		}
		// Remember Stripe's session ID (the webhook names it). Set once: a replay
		// gets the same session back, so there's nothing to overwrite.
		if _, err := pool.Exec(r.Context(), `
			UPDATE payment SET provider_ref = $2 WHERE id = $1 AND provider_ref IS NULL`, p.id, sessionID); err != nil {
			logFrom(r).Error("checkout: saving the Stripe session failed", "err", err, "payment_id", p.id)
			writeError(w, r, http.StatusInternalServerError, "could not start the payment")
			return
		}
		logFrom(r).Info("checkout started", "payment_id", p.id, "amount_cents", p.priceCents)
		writeJSON(w, http.StatusOK, map[string]any{"payment_id": p.id, "checkout_url": pageURL})
	}
}

type pendingPayment struct {
	id, currency           string
	priceCents, bonusCents int
}

// recordPendingPayment creates the payment for this purchase attempt, or finds
// the one this Idempotency-Key already made. The pack's price and bonus are
// copied onto it (the snapshot): what's promised now is what the webhook grants.
func recordPendingPayment(ctx context.Context, pool *pgxpool.Pool, accountID, key, packID string) (pendingPayment, error) {
	var p pendingPayment
	err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		var active bool
		err := tx.QueryRow(ctx, `
			SELECT price_cents, bonus_cents, currency, status = 'active' FROM credit_pack WHERE id = $1`, packID).
			Scan(&p.priceCents, &p.bonusCents, &p.currency, &active)
		if errors.Is(err, pgx.ErrNoRows) || (err == nil && !active) {
			return &bookingError{http.StatusConflict, "pack_unavailable", "that credit pack isn't on offer"}
		}
		if err != nil {
			return err
		}

		p.id = newUUIDv7()
		tag, err := tx.Exec(ctx, `
			INSERT INTO payment (id, account_id, provider, amount_cents, currency, idempotency_key, credit_pack_id, bonus_cents)
			VALUES ($1, $2, 'stripe', $3, $4, $5, $6, $7)
			ON CONFLICT (account_id, idempotency_key) DO NOTHING`,
			p.id, accountID, p.priceCents, p.currency, key, packID, p.bonusCents)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 1 {
			return nil // a new payment
		}
		// This key was used before: answer with that payment, if it's for the same pack.
		var samePack bool
		var status string
		err = tx.QueryRow(ctx, `
			SELECT id::text, amount_cents, bonus_cents, currency, credit_pack_id = $3, status
			FROM payment WHERE account_id = $1 AND idempotency_key = $2`, accountID, key, packID).
			Scan(&p.id, &p.priceCents, &p.bonusCents, &p.currency, &samePack, &status)
		if err != nil {
			return err
		}
		switch {
		case !samePack:
			return &bookingError{http.StatusUnprocessableEntity, "idempotency_key_reused",
				"this Idempotency-Key was already used for a different purchase"}
		case status != "pending":
			return &bookingError{http.StatusConflict, "already_paid", "this purchase is already finished"}
		}
		return nil
	})
	return p, err
}
