package main

// Everything that exists only on test sites: the dev Codespace (APP_ENV=dev)
// and https://dev.hablemos.live (APP_ENV=staging). Production sets no APP_ENV,
// so none of this is reachable there.

import (
	"context"
	"errors"
	"net/http"

	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	testCreditCents    = 5000  // one press of "Add test credit" = $50
	testCreditMaxCents = 50000 // no more presses once the balance reaches $500
)

// isTestSite reports whether APP_ENV marks a test site, where money is fake.
func isTestSite(appEnv string) bool {
	return appEnv == "dev" || appEnv == "staging"
}

// getEnv handles GET /env: which environment this is, so the page can show a
// "test site" banner. "production" when APP_ENV isn't set.
func getEnv(appEnv string) http.HandlerFunc {
	name := appEnv
	if name == "" {
		name = "production"
	}
	return func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"env": name, "test_site": isTestSite(appEnv)})
	}
}

// addTestCredit handles POST /test-credit (test sites only; wrapped in
// requireSignIn): adds $50 of fake credit as an 'adjustment' ledger entry, so
// testers can book without paying. Refused once the balance reaches $500.
func addTestCredit(pool *pgxpool.Pool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		me, _ := accountFrom(r)
		ctx := r.Context()
		var balance int
		err := pool.QueryRow(ctx, `
			SELECT COALESCE(SUM(amount_cents), 0) FROM credit_ledger
			WHERE account_id = $1 AND currency = 'USD'`, me.ID).Scan(&balance)
		if err != nil {
			logFrom(r).Error("test credit: balance query failed", "err", err)
			writeError(w, r, http.StatusInternalServerError, "could not add test credit")
			return
		}
		if balance >= testCreditMaxCents {
			writeErrorCode(w, r, http.StatusConflict, "test_credit_limit", "that's plenty of test credit already")
			return
		}
		_, err = pool.Exec(ctx, `
			INSERT INTO credit_ledger (id, account_id, entry_type, amount_cents, note)
			VALUES ($1, $2, 'adjustment', $3, 'test credit (test site)')`,
			newUUIDv7(), me.ID, testCreditCents)
		if err != nil {
			logFrom(r).Error("test credit: insert failed", "err", err)
			writeError(w, r, http.StatusInternalServerError, "could not add test credit")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"balance_cents": balance + testCreditCents})
	}
}

// runSeed loads the dev seed data (scripts/seed.sql): a teacher, classes,
// prices, sample students. It refuses to run outside a test site, as a second
// guard (the first: only staging's migration job passes --seed). The script
// is idempotent and moves class times relative to now, so rerunning it on
// every deploy keeps staging's classes in the future.
func runSeed(ctx context.Context, pool *pgxpool.Pool, appEnv, seedSQL string) error {
	if !isTestSite(appEnv) {
		return errors.New("refusing to load seed data: APP_ENV must be dev or staging")
	}
	// With no arguments, pgx sends the whole script as one "simple query", so its
	// own BEGIN ... COMMIT and all statements in between run as written.
	_, err := pool.Exec(ctx, seedSQL)
	return err
}
