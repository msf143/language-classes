package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func decodePrices(t *testing.T, rec *httptest.ResponseRecorder) pricesAnswer {
	t.Helper()
	var a pricesAnswer
	if err := json.Unmarshal(rec.Body.Bytes(), &a); err != nil {
		t.Fatalf("%d %s: %v", rec.Code, rec.Body, err)
	}
	return a
}

// pricesBody builds a PUT body from "kind/minutes/people=cents" items.
func pricesBody(items ...string) string {
	var parts []string
	for _, it := range items {
		var kind string
		var minutes, people, cents int
		fmt.Sscanf(strings.NewReplacer("/", " ", "=", " ").Replace(it), "%s %d %d %d", &kind, &minutes, &people, &cents)
		parts = append(parts, fmt.Sprintf(`{"kind":%q,"duration_minutes":%d,"party_size":%d,"price_per_person_cents":%d}`, kind, minutes, people, cents))
	}
	return `{"prices": [` + strings.Join(parts, ",") + `]}`
}

// GET and PUT /teacher/prices: versions instead of overwrites, unchanged prices
// left alone, removed prices closed, refusals, and booking seeing the new price.
func TestTeacherPrices(t *testing.T) {
	pool := migratedSchemaPool(t, 4)
	cookie, teacherID := newSignedInTeacher(t, pool)
	put := func(body string) *httptest.ResponseRecorder {
		return send(pool, putTeacherPrices(pool), "PUT", "/teacher/prices", body, "", cookie)
	}
	get := func() pricesAnswer {
		return decodePrices(t, send(pool, getTeacherPrices(pool), "GET", "/teacher/prices", "", "", cookie))
	}
	rows := func(where string) int {
		return count(t, pool, `SELECT count(*) FROM price_tier WHERE teacher_id = $1 AND `+where, teacherID)
	}

	if a := get(); len(a.Prices) != 0 || a.MaxPartySize != 4 || a.MinPriceCents != 100 || a.Currency != "USD" {
		t.Fatalf("a new teacher: %+v", a)
	}

	// First save: three prices.
	if rec := put(pricesBody("group/60/1=1500", "private/60/1=2500", "private/60/2=2000")); rec.Code != 200 || len(decodePrices(t, rec).Prices) != 3 {
		t.Fatalf("first save: %d %s", rec.Code, rec.Body)
	}

	// Second save: one unchanged, one changed, one left out.
	rec := put(pricesBody("group/60/1=1500", "private/60/1=2700"))
	got := decodePrices(t, rec).Prices
	if rec.Code != 200 || len(got) != 2 || got[1].PricePerPersonCents != 2700 {
		t.Fatalf("second save: %d %s", rec.Code, rec.Body)
	}
	tests := []struct {
		name  string
		where string
		want  int
	}{
		{"unchanged group price: still its first and only row", `kind = 'group'`, 1},
		{"changed private price: old row closed, new row open", `kind = 'private' AND party_size = 1`, 2},
		{"the old $25 row ends exactly where the $27 row starts", `kind = 'private' AND party_size = 1 AND price_per_person_cents = 2500
			AND upper(valid) = (SELECT lower(valid) FROM price_tier WHERE teacher_id = $1 AND price_per_person_cents = 2700)`, 1},
		{"left-out price: closed, nothing valid now", `kind = 'private' AND party_size = 2 AND valid @> now()`, 0},
		{"left-out price: its row is kept (history)", `kind = 'private' AND party_size = 2`, 1},
		{"valid now: two prices", `valid @> now()`, 2},
	}
	for _, tt := range tests {
		if n := rows(tt.where); n != tt.want {
			t.Errorf("%s: %d rows, want %d", tt.name, n, tt.want)
		}
	}

	t.Run("a booking now pays the new price", func(t *testing.T) {
		var cents int
		tx, err := pool.Begin(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(t.Context())
		if cents, _, err = priceFor(t.Context(), tx, teacherID, "private", 60, 1); err != nil || cents != 2700 {
			t.Errorf("priceFor: %d (err %v), want 2700", cents, err)
		}
	})

	t.Run("refused, and nothing changes", func(t *testing.T) {
		before := rows("true")
		for name, body := range map[string]string{
			"zero price":       pricesBody("group/60/1=0"),
			"over $500":        pricesBody("group/60/1=50001"),
			"90 minutes":       pricesBody("group/90/1=1500"),
			"party of 5":       pricesBody("private/60/5=1500"),
			"unknown kind":     pricesBody("course/60/1=1500"),
			"listed twice":     pricesBody("group/60/1=1500", "group/60/1=1600"),
			"no list at all":   `{}`,
			"an unknown field": `{"prices": [], "currency": "EUR"}`,
		} {
			if rec := put(body); rec.Code != http.StatusBadRequest {
				t.Errorf("%s: %d %s", name, rec.Code, rec.Body)
			}
		}
		if after := rows("true"); after != before {
			t.Errorf("rows %d → %d after refused saves", before, after)
		}
	})

	t.Run("an empty list removes every price", func(t *testing.T) {
		if rec := put(`{"prices": []}`); rec.Code != 200 || len(get().Prices) != 0 {
			t.Errorf("%d %s", rec.Code, rec.Body)
		}
	})
}

// Two saves at once (say, two browser tabs). The test plays the first: it
// locks the teacher row and changes a price, holding its transaction open
// while the real PUT starts. With the lock, the PUT waits, then sees the first
// save's result and builds on it: one valid price, the PUT's. Without it, both
// work from the same old state and the second insert collides with the first
// (the EXCLUDE constraint), so the PUT fails.
func TestTeacherPricesRace(t *testing.T) {
	pool := migratedSchemaPool(t, 4)
	ctx := t.Context()
	cookie, teacherID := newSignedInTeacher(t, pool)
	if rec := send(pool, putTeacherPrices(pool), "PUT", "/teacher/prices", pricesBody("private/60/1=2500"), "", cookie); rec.Code != 200 {
		t.Fatalf("setup: %d %s", rec.Code, rec.Body)
	}

	gate, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer gate.Rollback(ctx)
	for _, sql := range []string{
		`SELECT 1 FROM teacher WHERE id = $1 FOR UPDATE`,
		`UPDATE price_tier SET valid = tstzrange(lower(valid), now()) WHERE teacher_id = $1 AND valid @> now()`,
		`INSERT INTO price_tier (id, teacher_id, kind, duration_minutes, party_size, price_per_person_cents, valid)
		 VALUES ($2, $1, 'private', 60, 1, 2600, tstzrange(now(), NULL))`,
	} {
		args := []any{teacherID}
		if strings.Contains(sql, "$2") {
			args = append(args, newUUIDv7())
		}
		if _, err := gate.Exec(ctx, sql, args...); err != nil {
			t.Fatal(err)
		}
	}

	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		done <- send(pool, putTeacherPrices(pool), "PUT", "/teacher/prices", pricesBody("private/60/1=2700"), "", cookie)
	}()
	waitForLockWaiters(t, pool, 1)
	if err := gate.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	rec := <-done

	if rec.Code != 200 {
		t.Errorf("the second save failed: %d %s", rec.Code, rec.Body)
	}
	if n := count(t, pool, `SELECT count(*) FROM price_tier WHERE teacher_id = $1 AND valid @> now()`, teacherID); n != 1 {
		t.Errorf("%d prices valid now, want 1", n)
	}
	if n := count(t, pool, `SELECT count(*) FROM price_tier WHERE teacher_id = $1 AND valid @> now() AND price_per_person_cents = 2700`, teacherID); n != 1 {
		t.Error("the valid price isn't the last save's $27")
	}
}
