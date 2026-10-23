package main

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
)

// GET /credit-packs lists the packs migration 000006 created, in order, with
// the credit each one gives; retired packs are left out.
func TestListCreditPacks(t *testing.T) {
	pool := migratedSchemaPool(t, 4) // exactly what the migrations create
	if _, err := pool.Exec(context.Background(), `
		INSERT INTO credit_pack (id, price_cents, bonus_cents, status, sort_order)
		VALUES ($1, 7500, 1000, 'retired', 0)`, newUUIDv7()); err != nil {
		t.Fatal(err)
	}

	rec := call(listCreditPacks(pool), "GET", "/credit-packs", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d, body %s", rec.Code, rec.Body)
	}
	var packs []creditPackDTO
	if err := json.Unmarshal(rec.Body.Bytes(), &packs); err != nil {
		t.Fatal(err)
	}
	want := []struct{ price, bonus, credit int }{{2500, 0, 2500}, {5000, 500, 5500}, {10000, 1500, 11500}}
	if len(packs) != len(want) {
		t.Fatalf("packs %+v; want the three active ones (not the retired $75 pack)", packs)
	}
	for i, w := range want {
		p := packs[i]
		if p.PriceCents != w.price || p.BonusCents != w.bonus || p.CreditCents != w.credit || p.Currency != "USD" {
			t.Errorf("pack %d = %+v; want pay %d, bonus %d, credit %d USD", i, p, w.price, w.bonus, w.credit)
		}
	}
}
