package main

import (
	"context"
	"net/http"
	"strings"
	"testing"
)

// `api admin`: grant (creating the account if needed), grant again, revoke;
// /me reports the flag; requireAdmin lets admins through and nobody else.
func TestAdmins(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	isAdmin := func(id string) bool {
		var b bool
		pool.QueryRow(ctx, `SELECT is_admin FROM account WHERE id = $1`, id).Scan(&b)
		return b
	}

	t.Run("a new person: account created, admin", func(t *testing.T) {
		email := testEmail()
		id, created, err := setAdmin(ctx, pool, " "+email+" ", true)
		if err != nil || !created || !isAdmin(id) {
			t.Errorf("id %s created %v admin %v err %v", id, created, isAdmin(id), err)
		}
	})

	accountID, email := newAccount(t, pool)
	cookie := signIn(t, pool, email)
	behindAdmin := func() int {
		return call(requireAdmin(pool, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) }), "GET", "/admin/x", cookie).Code
	}
	if got := behindAdmin(); got != http.StatusForbidden {
		t.Errorf("before: %d, want 403", got)
	}

	for range 2 { // granting twice is the same as once
		if id, created, err := setAdmin(ctx, pool, email, true); err != nil || id != accountID || created {
			t.Fatalf("grant: %s %v %v", id, created, err)
		}
	}
	if got := behindAdmin(); got != http.StatusNoContent {
		t.Errorf("admin: %d, want through (204)", got)
	}
	if rec := call(requireSignIn(pool, getMe(pool)), "GET", "/me", cookie); !strings.Contains(rec.Body.String(), `"is_admin":true`) {
		t.Errorf("/me doesn't say is_admin: %s", rec.Body)
	}

	if _, _, err := setAdmin(ctx, pool, email, false); err != nil || isAdmin(accountID) {
		t.Errorf("revoke: err %v, still admin %v", err, isAdmin(accountID))
	}
	if got := behindAdmin(); got != http.StatusForbidden {
		t.Errorf("after revoke: %d, want 403 at once", got)
	}

	t.Run("refused", func(t *testing.T) {
		if _, _, err := setAdmin(ctx, pool, "not-an-email", true); err == nil {
			t.Error("a non-email was accepted")
		}
		if _, _, err := setAdmin(ctx, pool, testEmail(), false); err == nil {
			t.Error("revoking from someone without an account should fail")
		}
		if got := call(requireAdmin(pool, func(w http.ResponseWriter, r *http.Request) {}), "GET", "/admin/x", nil).Code; got != 401 {
			t.Errorf("signed out: %d, want 401", got)
		}
	})
}
