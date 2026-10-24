package main

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
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

// credit adds a ledger entry dated `ago` in the past (the ledger is
// append-only, so a test can't backdate an entry; it writes it backdated).
func credit(t *testing.T, pool *pgxpool.Pool, accountID string, cents int, ago string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `
		INSERT INTO credit_ledger (id, account_id, entry_type, amount_cents, note, created_at)
		VALUES ($1, $2, 'adjustment', $3, 'test', now() - $4::interval)`, newUUIDv7(), accountID, cents, ago); err != nil {
		t.Fatal(err)
	}
}

// One account per situation; then the two checks run (twice, to show they
// don't repeat themselves) and each account is checked.
func TestCreditExpiry(t *testing.T) {
	pool := migratedSchemaPool(t, 4)
	ctx := context.Background()
	account := func() string { id, _ := newAccount(t, pool); return id }

	active := account() // used last month
	credit(t, pool, active, 5000, "1 month")
	warned := account() // expires in 10 days
	credit(t, pool, warned, 3000, "24 months -10 days")
	due := account() // expired a month ago
	credit(t, pool, due, 2500, "26 months")
	credit(t, pool, due, 1500, "25 months")
	empty := account() // old, but nothing left
	credit(t, pool, empty, 5000, "26 months")
	credit(t, pool, empty, -5000, "25 months")
	revived := account() // old credit, but active last month
	credit(t, pool, revived, 4000, "30 months")
	credit(t, pool, revived, 1000, "1 month")

	for run := 1; run <= 2; run++ {
		queued, err := queueExpiryReminders(ctx, pool)
		if err != nil {
			t.Fatal(err)
		}
		expired, err := expireCredit(ctx, pool)
		if err != nil {
			t.Fatal(err)
		}
		if want := map[int]int{1: 1, 2: 0}[run]; queued != want || expired != want {
			t.Errorf("run %d: %d reminders queued, %d accounts expired; want %d and %d", run, queued, expired, want, want)
		}
	}

	tests := []struct {
		name                 string
		id                   string
		balance, reminders   int
		expiries, expiryMail int
	}{
		{"active last month: untouched", active, 5000, 0, 0, 0},
		{"10 days left: one reminder", warned, 3000, 1, 0, 0},
		{"past due: whole balance expired", due, 0, 0, 1, 1},
		{"nothing left: nothing to expire", empty, 0, 0, 0, 0},
		{"old credit, recent activity: untouched", revived, 5000, 0, 0, 0},
	}
	for _, tt := range tests {
		got := []int{
			balance(t, pool, tt.id),
			count(t, pool, `SELECT count(*) FROM outbox_event WHERE event_type = 'credit_expiry_reminder' AND aggregate_id = $1`, tt.id),
			count(t, pool, `SELECT count(*) FROM credit_ledger WHERE entry_type = 'expiry' AND account_id = $1`, tt.id),
			count(t, pool, `SELECT count(*) FROM outbox_event WHERE event_type = 'credit_expired' AND aggregate_id = $1`, tt.id),
		}
		want := []int{tt.balance, tt.reminders, tt.expiries, tt.expiryMail}
		if !slices.Equal(got, want) {
			t.Errorf("%s: balance, reminders, expiry entries, expiry emails = %v, want %v", tt.name, got, want)
		}
	}

	t.Run("the expiry entry and its email say what happened", func(t *testing.T) {
		var amount int
		var note, payload string
		if err := pool.QueryRow(ctx, `SELECT amount_cents, note FROM credit_ledger WHERE entry_type = 'expiry' AND account_id = $1`, due).Scan(&amount, &note); err != nil {
			t.Fatal(err)
		}
		if err := pool.QueryRow(ctx, `SELECT payload->>'expired_cents' FROM outbox_event WHERE event_type = 'credit_expired' AND aggregate_id = $1`, due).Scan(&payload); err != nil {
			t.Fatal(err)
		}
		if amount != -4000 || !strings.HasPrefix(note, "no credit activity since ") || payload != "4000" {
			t.Errorf("entry %d %q, email payload expired_cents %s; want -4000, a note, 4000", amount, note, payload)
		}
	})

	t.Run("a new quiet stretch gets a new reminder", func(t *testing.T) {
		// Activity after the first reminder (a top-up 10 days later), and that
		// has gone quiet too: it now expires in 20 days, inside the 30-day window.
		credit(t, pool, warned, 500, "24 months -20 days")
		if queued, err := queueExpiryReminders(ctx, pool); err != nil || queued != 1 {
			t.Errorf("queued %d (err %v), want 1 new reminder", queued, err)
		}
	})
}

// The expiry and a booking at the same moment. The test plays the booking: it
// locks the account (as booking does), writes a spend, and holds the
// transaction open while the expiry starts. With the account lock, the expiry
// waits, then sees the new activity and expires nothing. Without it, the
// expiry reads the old balance, expires all $40, and the spend's $15 then
// commits on top: a balance of -$15.
func TestCreditExpiryRace(t *testing.T) {
	pool := migratedSchemaPool(t, 4)
	ctx := context.Background()
	id, _ := newAccount(t, pool)
	credit(t, pool, id, 4000, "25 months")

	gate, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer gate.Rollback(ctx)
	if _, err := gate.Exec(ctx, `SELECT 1 FROM account WHERE id = $1 FOR UPDATE`, id); err != nil {
		t.Fatal(err)
	}
	if _, err := gate.Exec(ctx, `
		INSERT INTO credit_ledger (id, account_id, entry_type, amount_cents, note)
		VALUES ($1, $2, 'adjustment', -1500, 'stands in for a booking spend')`, newUUIDv7(), id); err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)
	go func() { _, err := expireCredit(ctx, pool); done <- err }()
	// Wait until the expiry is blocked on the account lock, or has finished
	// (which is what happens without the lock).
	deadline := time.Now().Add(5 * time.Second)
	for count(t, pool, `SELECT count(*) FROM pg_stat_activity WHERE datname = current_database() AND wait_event_type = 'Lock'`) == 0 &&
		len(done) == 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if err := gate.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}

	if b := balance(t, pool, id); b != 2500 {
		t.Errorf("balance %d, want 2500 (the $15 spend counts as activity: nothing expires)", b)
	}
}
