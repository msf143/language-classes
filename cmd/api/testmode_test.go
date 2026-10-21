package main

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/msf143/language-classes/scripts"
)

func TestGetEnv(t *testing.T) {
	tests := []struct {
		appEnv, wantEnv string
		wantTestSite    bool
	}{
		{"", "production", false},
		{"staging", "staging", true},
		{"dev", "dev", true},
	}
	for _, tt := range tests {
		t.Run(tt.wantEnv, func(t *testing.T) {
			rec := call(getEnv(tt.appEnv), "GET", "/env", nil)
			var got struct {
				Env      string `json:"env"`
				TestSite bool   `json:"test_site"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
				t.Fatal(err)
			}
			if got.Env != tt.wantEnv || got.TestSite != tt.wantTestSite {
				t.Errorf("got %+v, want env %q test_site %v", got, tt.wantEnv, tt.wantTestSite)
			}
		})
	}
}

func TestAddTestCredit(t *testing.T) {
	pool := testPool(t)
	h := requireSignIn(pool, addTestCredit(pool))

	s := newStudent(t, pool, 0, false)
	rec := call(h, "POST", "/test-credit", s.cookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d, body %s", rec.Code, rec.Body)
	}
	if got := balance(t, pool, s.accountID); got != testCreditCents {
		t.Errorf("balance %d, want %d", got, testCreditCents)
	}

	rich := newStudent(t, pool, testCreditMaxCents, false)
	rec = call(h, "POST", "/test-credit", rich.cookie)
	if rec.Code != http.StatusConflict || errorCode(t, rec) != "test_credit_limit" {
		t.Errorf("at the limit: status %d, body %s", rec.Code, rec.Body)
	}
	if got := balance(t, pool, rich.accountID); got != testCreditMaxCents {
		t.Errorf("balance changed at the limit: %d", got)
	}

	if rec := call(h, "POST", "/test-credit", nil); rec.Code != http.StatusUnauthorized {
		t.Errorf("signed out: status %d, want 401", rec.Code)
	}
}

// The seed must never load on production. The refusal comes before any
// database access, so no pool is needed to check it.
func TestRunSeedRefusesProduction(t *testing.T) {
	if err := runSeed(context.Background(), nil, "", scripts.Seed); err == nil {
		t.Fatal("runSeed with APP_ENV unset (production) should refuse")
	}
}

// The embedded seed runs, twice (it must be idempotent), on the dev database.
func TestRunSeed(t *testing.T) {
	pool := testPool(t)
	for i := range 2 {
		if err := runSeed(context.Background(), pool, "dev", scripts.Seed); err != nil {
			t.Fatalf("run %d: %v", i+1, err)
		}
	}
	if n := count(t, pool, `SELECT count(*) FROM teacher WHERE id = '01900000-0000-7000-8000-0000000000a1'`); n != 1 {
		t.Errorf("seed teacher rows: %d, want 1", n)
	}
}

// On test sites, any signed-in tester can take over the seed teacher profile.
func TestBecomeTestTeacher(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	if err := runSeed(ctx, pool, "dev", scripts.Seed); err != nil { // the seed teacher must exist
		t.Fatal(err)
	}
	var originalAccount string
	if err := pool.QueryRow(ctx, `SELECT account_id::text FROM teacher WHERE id = $1`, seedTeacherID).Scan(&originalAccount); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { // hand the profile back, so dev keeps working as teacher@example.com
		if _, err := pool.Exec(context.Background(), `UPDATE teacher SET account_id = $2 WHERE id = $1`, seedTeacherID, originalAccount); err != nil {
			t.Errorf("restoring the seed teacher: %v", err)
		}
	})

	tester := newStudent(t, pool, 0, false)
	if rec := call(requireSignIn(pool, becomeTestTeacher(pool)), "POST", "/test-teacher", tester.cookie); rec.Code != http.StatusOK {
		t.Fatalf("status %d, body %s", rec.Code, rec.Body)
	}
	if rec := call(requireTeacher(pool, listAvailability(pool)), "GET", "/teacher/availability", tester.cookie); rec.Code != http.StatusOK {
		t.Errorf("the tester should now pass requireTeacher, got %d", rec.Code)
	}

	// Someone who is already a (different) teacher can't also take this one.
	otherTeacher, _ := newSignedInTeacher(t, pool)
	if rec := call(requireSignIn(pool, becomeTestTeacher(pool)), "POST", "/test-teacher", otherTeacher); rec.Code != http.StatusConflict {
		t.Errorf("another teacher: status %d, want 409", rec.Code)
	}
}
