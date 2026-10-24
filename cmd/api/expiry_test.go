package main

import (
	"context"
	"testing"
)

// Migration 000007: one credit_policy row (24 months, warn 30 days before),
// and the ledger accepts 'expiry' entries only as debits.
func TestCreditExpirySchema(t *testing.T) {
	pool := migratedSchemaPool(t, 2)
	ctx := context.Background()

	var months, warnDays int
	if err := pool.QueryRow(ctx, `SELECT expire_after_months, warn_days_before FROM credit_policy`).Scan(&months, &warnDays); err != nil {
		t.Fatal(err)
	}
	if months != 24 || warnDays != 30 {
		t.Errorf("policy %d months, warn %d days; want 24 and 30", months, warnDays)
	}

	s := newStudent(t, pool, 0, false)
	tests := []struct {
		name   string
		sql    string
		wantOK bool
	}{
		{"a second policy row", `INSERT INTO credit_policy (expire_after_months, warn_days_before) VALUES (12, 30)`, false},
		{"a zero period", `UPDATE credit_policy SET expire_after_months = 0`, false},
		{"an expiry debit", `INSERT INTO credit_ledger (id, account_id, entry_type, amount_cents) VALUES ('` + newUUIDv7() + `', '` + s.accountID + `', 'expiry', -500)`, true},
		{"an expiry credit", `INSERT INTO credit_ledger (id, account_id, entry_type, amount_cents) VALUES ('` + newUUIDv7() + `', '` + s.accountID + `', 'expiry', 500)`, false},
	}
	for _, tt := range tests {
		_, err := pool.Exec(ctx, tt.sql)
		if (err == nil) != tt.wantOK {
			t.Errorf("%s: err = %v, want ok = %v", tt.name, err, tt.wantOK)
		}
	}
}
