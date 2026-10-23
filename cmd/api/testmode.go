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
// "test site" banner ("production" when APP_ENV isn't set), and whether buying
// credit works here, so the page shows Buy credit only where it does.
func getEnv(appEnv string, payments bool) http.HandlerFunc {
	name := appEnv
	if name == "" {
		name = "production"
	}
	return func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"env": name, "test_site": isTestSite(appEnv), "payments": payments})
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

// seedTeacherID is the dev/staging teacher created by scripts/seed.sql.
const seedTeacherID = "01900000-0000-7000-8000-0000000000a1"

// becomeTestTeacher handles POST /test-teacher (test sites only; wrapped in
// requireSignIn): it links the seed teacher profile (with its prices, classes
// and availability) to the signed-in account, so a tester can try the teacher's
// page with their own login. The seed teacher's email is teacher@example.com,
// which nobody receives. One test teacher: whoever pressed last is it.
func becomeTestTeacher(pool *pgxpool.Pool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		me, _ := accountFrom(r)
		tag, err := pool.Exec(r.Context(), `
			UPDATE teacher SET account_id = $2, status = 'active'
			WHERE id = $1
			  AND NOT EXISTS (SELECT 1 FROM teacher WHERE account_id = $2 AND id <> $1)`,
			seedTeacherID, me.ID)
		if err != nil {
			logFrom(r).Error("become test teacher failed", "err", err)
			writeError(w, r, http.StatusInternalServerError, "could not make you the test teacher")
			return
		}
		if tag.RowsAffected() == 0 {
			// No seed teacher here, or this account is already a different teacher.
			writeErrorCode(w, r, http.StatusConflict, "not_possible", "you can't become the test teacher with this account")
			return
		}
		logFrom(r).Info("test teacher taken over", "account_id", me.ID)
		writeJSON(w, http.StatusOK, map[string]any{"teacher_id": seedTeacherID})
	}
}
