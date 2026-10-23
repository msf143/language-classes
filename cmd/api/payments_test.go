package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
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

const pack50 = "01a20000-0000-7000-8000-000000005000" // pay $50, get $55 (migration 000006)

// fakeStripe stands in for api.stripe.com: it records each request and
// answers with a session whose ID follows from the Idempotency-Key, so the
// same key gets the same session back, as with the real Stripe.
type fakeStripe struct {
	mu       sync.Mutex
	requests []fakeStripeRequest
	fail     bool
	url      string
}

type fakeStripeRequest struct {
	path, user, idempotencyKey string
	form                       url.Values
}

func newFakeStripe(t *testing.T) *fakeStripe {
	f := &fakeStripe{}
	f.url = httptestServer(t, func(w http.ResponseWriter, r *http.Request) {
		user, _, _ := r.BasicAuth()
		r.ParseForm()
		f.mu.Lock()
		f.requests = append(f.requests, fakeStripeRequest{r.URL.Path, user, r.Header.Get("Idempotency-Key"), r.PostForm})
		fail := f.fail
		f.mu.Unlock()
		if fail {
			w.WriteHeader(http.StatusInternalServerError)
			w.Write([]byte(`{"error": {"message": "something went wrong"}}`))
			return
		}
		id := "cs_test_" + strings.TrimPrefix(r.Header.Get("Idempotency-Key"), "checkout-")
		w.Write([]byte(`{"id": "` + id + `", "url": "https://checkout.stripe.test/` + id + `"}`))
	})
	return f
}

func startCheckoutReq(pool *pgxpool.Pool, s *stripeClient, cookie *http.Cookie, key, packID string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("POST", "/payments/checkout", strings.NewReader(`{"pack_id": "`+packID+`"}`))
	req.Header.Set("Idempotency-Key", key)
	if cookie != nil {
		req.AddCookie(cookie)
	}
	rec := httptest.NewRecorder()
	requireSignIn(pool, startCheckout(pool, s, "https://hablemos.live"))(rec, req)
	return rec
}

func TestStartCheckout(t *testing.T) {
	pool := migratedSchemaPool(t, 4)
	ctx := context.Background()
	fake := newFakeStripe(t)
	stripe := newStripe("sk_test_fake", fake.url)
	s := newStudent(t, pool, 0, false)

	type answer struct {
		PaymentID   string `json:"payment_id"`
		CheckoutURL string `json:"checkout_url"`
	}
	decode := func(rec *httptest.ResponseRecorder) answer {
		var a answer
		json.Unmarshal(rec.Body.Bytes(), &a)
		return a
	}

	rec := startCheckoutReq(pool, stripe, s.cookie, "k1", pack50)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d, body %s", rec.Code, rec.Body)
	}
	first := decode(rec)

	t.Run("a pending payment with the pack's price and bonus, and Stripe's session", func(t *testing.T) {
		var amount, bonus int
		var status, provider, ref, pack string
		err := pool.QueryRow(ctx, `
			SELECT amount_cents, bonus_cents, status, provider, provider_ref, credit_pack_id::text
			FROM payment WHERE id = $1 AND account_id = $2`, first.PaymentID, s.accountID).
			Scan(&amount, &bonus, &status, &provider, &ref, &pack)
		if err != nil {
			t.Fatal(err)
		}
		if amount != 5000 || bonus != 500 || status != "pending" || provider != "stripe" || pack != pack50 ||
			ref != "cs_test_"+first.PaymentID || first.CheckoutURL != "https://checkout.stripe.test/"+ref {
			t.Errorf("payment: %d %d %s %s %s %s; url %s", amount, bonus, status, provider, ref, pack, first.CheckoutURL)
		}
	})

	t.Run("what Stripe was asked for", func(t *testing.T) {
		r := fake.requests[0]
		want := map[string]string{
			"mode":                                   "payment",
			"client_reference_id":                    first.PaymentID,
			"line_items[0][price_data][unit_amount]": "5000",
			"line_items[0][price_data][currency]":    "usd",
			"customer_email":                         accountEmail(t, pool, s.accountID),
			"success_url":                            "https://hablemos.live/?paid=" + first.PaymentID,
			"line_items[0][price_data][product_data][name]": "Hablemos credit: $55.00 · Crédito Hablemos: 55,00 US$",
		}
		for k, v := range want {
			if got := r.form.Get(k); got != v {
				t.Errorf("%s = %q, want %q", k, got, v)
			}
		}
		if r.path != "/v1/checkout/sessions" || r.user != "sk_test_fake" || r.idempotencyKey != "checkout-"+first.PaymentID {
			t.Errorf("request %s, auth user %q, idempotency key %q", r.path, r.user, r.idempotencyKey)
		}
	})

	t.Run("the same key again: the same payment and the same Stripe page", func(t *testing.T) {
		again := decode(startCheckoutReq(pool, stripe, s.cookie, "k1", pack50))
		if again != first {
			t.Errorf("replay %+v, want %+v", again, first)
		}
		if n := count(t, pool, `SELECT count(*) FROM payment WHERE account_id = $1`, s.accountID); n != 1 {
			t.Errorf("%d payments, want 1", n)
		}
	})

	t.Run("refusals", func(t *testing.T) {
		retired := newUUIDv7()
		pool.Exec(ctx, `INSERT INTO credit_pack (id, price_cents, status) VALUES ($1, 7500, 'retired')`, retired)
		other := newStudent(t, pool, 0, false)
		tests := []struct {
			name       string
			stripe     *stripeClient
			cookie     *http.Cookie
			key, pack  string
			wantStatus int
			wantCode   string
		}{
			{"same key, other pack", stripe, s.cookie, "k1", "01a20000-0000-7000-8000-000000002500", 422, "idempotency_key_reused"},
			{"a retired pack", stripe, other.cookie, "k", retired, 409, "pack_unavailable"},
			{"an unknown pack", stripe, other.cookie, "k", newUUIDv7(), 409, "pack_unavailable"},
			{"no Stripe key configured", nil, other.cookie, "k", pack50, 503, "payments_unavailable"},
			{"signed out", stripe, nil, "k", pack50, 401, ""},
		}
		for _, tt := range tests {
			rec := startCheckoutReq(pool, tt.stripe, tt.cookie, tt.key, tt.pack)
			if rec.Code != tt.wantStatus || (tt.wantCode != "" && errorCode(t, rec) != tt.wantCode) {
				t.Errorf("%s: %d %s, want %d %s", tt.name, rec.Code, rec.Body, tt.wantStatus, tt.wantCode)
			}
		}
	})

	t.Run("a finished purchase isn't reopened", func(t *testing.T) {
		pool.Exec(ctx, `UPDATE payment SET status = 'succeeded' WHERE id = $1`, first.PaymentID)
		rec := startCheckoutReq(pool, stripe, s.cookie, "k1", pack50)
		if rec.Code != 409 || errorCode(t, rec) != "already_paid" {
			t.Errorf("%d %s, want 409 already_paid", rec.Code, rec.Body)
		}
	})

	t.Run("Stripe failing: 502, and the payment stays pending without a session", func(t *testing.T) {
		fake.fail = true
		defer func() { fake.fail = false }()
		other := newStudent(t, pool, 0, false)
		rec := startCheckoutReq(pool, stripe, other.cookie, "k", pack50)
		if rec.Code != 502 || errorCode(t, rec) != "payment_provider_error" {
			t.Errorf("%d %s, want 502 payment_provider_error", rec.Code, rec.Body)
		}
		if n := count(t, pool, `SELECT count(*) FROM payment WHERE account_id = $1 AND status = 'pending' AND provider_ref IS NULL`, other.accountID); n != 1 {
			t.Errorf("%d pending payments without a session, want 1", n)
		}
	})
}
