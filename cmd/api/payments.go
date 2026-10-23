package main

// Buying credit: the packs on offer, and (from step 6b) paying for one with
// Stripe Checkout.

import (
	"net/http"

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
