package main

// The teacher's prices (step 7c). A price is never overwritten: changing one
// closes the current price_tier row's validity window at now() and inserts the
// new price from that same instant (both in one transaction, so the windows
// touch without overlapping). Bookings snapshot what they paid, and the
// EXCLUDE constraint on price_tier keeps one price per cell at any moment.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	minPriceCents = 100   // $1.00: a $0 booking would need a $0 ledger entry, which the ledger forbids
	maxPriceCents = 50000 // $500.00 per person: a typo guard
)

// priceCellDTO is one cell of the price table: the price per person for one
// kind × length × party size. (priceDTO, in sessions.go, is the catalog's
// shorter "price per party size".)
type priceCellDTO struct {
	Kind                string `json:"kind"` // "group" or "private"
	DurationMinutes     int    `json:"duration_minutes"`
	PartySize           int    `json:"party_size"`
	PricePerPersonCents int    `json:"price_per_person_cents"`
}

type pricesAnswer struct {
	Prices         []priceCellDTO `json:"prices"` // valid now
	Currency       string         `json:"currency"`
	AllowedLengths []int          `json:"allowed_lengths"`
	MaxPartySize   int            `json:"max_party_size"`
	MinPriceCents  int            `json:"min_price_cents"`
	MaxPriceCents  int            `json:"max_price_cents"`
}

// getTeacherPrices handles GET /teacher/prices (wrapped in requireTeacher).
func getTeacherPrices(pool *pgxpool.Pool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		prices, err := currentPrices(r.Context(), pool, teacherFrom(r))
		if err != nil {
			logFrom(r).Error("teacher prices: load failed", "err", err)
			writeError(w, r, http.StatusInternalServerError, "could not load your prices")
			return
		}
		writeJSON(w, http.StatusOK, newPricesAnswer(prices))
	}
}

func newPricesAnswer(prices []priceCellDTO) pricesAnswer {
	return pricesAnswer{prices, "USD", allowedLengths, maxPartySize, minPriceCents, maxPriceCents}
}

// querier is what both a pool and a transaction can do here, so the same
// function reads prices before or inside a transaction.
type querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

func currentPrices(ctx context.Context, q querier, teacherID string) ([]priceCellDTO, error) {
	rows, err := q.Query(ctx, `
		SELECT kind, duration_minutes, party_size, price_per_person_cents FROM price_tier
		WHERE teacher_id = $1 AND valid @> now() AND currency = 'USD'
		ORDER BY kind, duration_minutes, party_size`, teacherID)
	if err != nil {
		return nil, err
	}
	prices, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (priceCellDTO, error) {
		var p priceCellDTO
		err := row.Scan(&p.Kind, &p.DurationMinutes, &p.PartySize, &p.PricePerPersonCents)
		return p, err
	})
	if prices == nil {
		prices = []priceCellDTO{} // [] rather than null in JSON
	}
	return prices, err
}

// putTeacherPrices handles PUT /teacher/prices {"prices": [...]} (wrapped in
// requireTeacher): the complete new price list. Per cell: unchanged → nothing;
// changed → close the old row, insert the new; missing → close (no price).
func putTeacherPrices(pool *pgxpool.Pool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Prices []priceCellDTO `json:"prices"`
		}
		dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 32<<10))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&body); err != nil || body.Prices == nil {
			writeError(w, r, http.StatusBadRequest, `body must be JSON like {"prices": [{"kind", "duration_minutes", "party_size", "price_per_person_cents"}]}`)
			return
		}
		if msg := checkPrices(body.Prices); msg != "" {
			writeErrorCode(w, r, http.StatusBadRequest, "invalid_prices", msg)
			return
		}

		teacherID := teacherFrom(r)
		var changed, closed int
		err := pgx.BeginFunc(r.Context(), pool, func(tx pgx.Tx) error {
			// One save at a time per teacher (two tabs saving at once take turns).
			if _, err := tx.Exec(r.Context(), `SELECT 1 FROM teacher WHERE id = $1 FOR UPDATE`, teacherID); err != nil {
				return err
			}
			current, err := currentPrices(r.Context(), tx, teacherID)
			if err != nil {
				return err
			}
			type cell struct {
				kind            string
				minutes, people int
			}
			now := map[cell]int{}
			for _, p := range current {
				now[cell{p.Kind, p.DurationMinutes, p.PartySize}] = p.PricePerPersonCents
			}
			want := map[cell]int{}
			for _, p := range body.Prices {
				want[cell{p.Kind, p.DurationMinutes, p.PartySize}] = p.PricePerPersonCents
			}
			// closeCell ends a cell's current price at now() (the same instant the new one starts).
			closeCell := func(c cell) error {
				_, err := tx.Exec(r.Context(), `
					UPDATE price_tier SET valid = tstzrange(lower(valid), now())
					WHERE teacher_id = $1 AND kind = $2 AND duration_minutes = $3 AND party_size = $4
					  AND valid @> now()`, teacherID, c.kind, c.minutes, c.people)
				return err
			}
			for c, cents := range want {
				old, had := now[c]
				if had && old == cents {
					continue // unchanged: no new version
				}
				if had {
					if err := closeCell(c); err != nil {
						return err
					}
				}
				if _, err := tx.Exec(r.Context(), `
					INSERT INTO price_tier (id, teacher_id, kind, duration_minutes, party_size, price_per_person_cents, currency, valid)
					VALUES ($1, $2, $3, $4, $5, $6, 'USD', tstzrange(now(), NULL))`,
					newUUIDv7(), teacherID, c.kind, c.minutes, c.people, cents); err != nil {
					return err
				}
				changed++
			}
			for c := range now {
				if _, keep := want[c]; !keep {
					if err := closeCell(c); err != nil {
						return err
					}
					closed++
				}
			}
			return nil
		})
		if err != nil {
			logFrom(r).Error("teacher prices: save failed", "err", err)
			writeError(w, r, http.StatusInternalServerError, "could not save your prices")
			return
		}
		logFrom(r).Info("teacher prices saved", "teacher_id", teacherID, "changed", changed, "removed", closed)
		saved, err := currentPrices(r.Context(), pool, teacherID)
		if err != nil {
			logFrom(r).Error("teacher prices: reload failed", "err", err)
			writeError(w, r, http.StatusInternalServerError, "saved, but could not load your prices")
			return
		}
		writeJSON(w, http.StatusOK, newPricesAnswer(saved))
	}
}

// checkPrices returns what's wrong with a price list, or "" if it's fine.
func checkPrices(prices []priceCellDTO) string {
	seen := map[[3]any]bool{}
	for _, p := range prices {
		switch {
		case p.Kind != "group" && p.Kind != "private":
			return `kind must be "group" or "private"`
		case !slices.Contains(allowedLengths, p.DurationMinutes):
			return fmt.Sprintf("duration_minutes must be one of %v", allowedLengths)
		case p.PartySize < 1 || p.PartySize > maxPartySize:
			return fmt.Sprintf("party_size must be 1 to %d", maxPartySize)
		case p.PricePerPersonCents < minPriceCents || p.PricePerPersonCents > maxPriceCents:
			return fmt.Sprintf("price_per_person_cents must be %d to %d", minPriceCents, maxPriceCents)
		}
		key := [3]any{p.Kind, p.DurationMinutes, p.PartySize}
		if seen[key] {
			return fmt.Sprintf("%s %d min for %d listed twice", p.Kind, p.DurationMinutes, p.PartySize)
		}
		seen[key] = true
	}
	return ""
}
