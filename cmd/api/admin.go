package main

// Admin routes (step 7f): seeing and managing teachers. An admin is an account
// with is_admin set (by the command `api admin`, never through the website).

import (
	"net/http"

	"github.com/jackc/pgx/v5/pgxpool"
)

// requireAdmin wraps a handler for admins only: signed out → 401 (from
// requireSignIn), signed in but not an admin → 403 not_admin. The flag comes
// with the signed-in account (accountColumns), read fresh on every request,
// so revoking it takes effect at once.
func requireAdmin(pool *pgxpool.Pool, next http.HandlerFunc) http.HandlerFunc {
	return requireSignIn(pool, func(w http.ResponseWriter, r *http.Request) {
		if a, _ := accountFrom(r); a == nil || !a.IsAdmin {
			writeErrorCode(w, r, http.StatusForbidden, "not_admin", "this is for admins only")
			return
		}
		next(w, r)
	})
}
