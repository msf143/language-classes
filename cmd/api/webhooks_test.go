package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

const testWebhookSecret = "whsec_test_secret"

// signStripe builds a Stripe-Signature header the way Stripe does.
func signStripe(secret string, at time.Time, body string) string {
	t := fmt.Sprint(at.Unix())
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(t + "." + body))
	return "t=" + t + ",v1=" + hex.EncodeToString(mac.Sum(nil))
}

func TestVerifyStripeSignature(t *testing.T) {
	now := time.Now()
	body := `{"id": "evt_1"}`
	valid := signStripe(testWebhookSecret, now, body)
	tests := []struct {
		name, header, body string
		ok                 bool
	}{
		{"valid", valid, body, true},
		{"two v1 values, one right (Stripe does this while rotating secrets)",
			strings.Replace(valid, ",v1=", ",v1=00ff,v1=", 1), body, true},
		{"signed with another secret", signStripe("whsec_other", now, body), body, false},
		{"body changed after signing", valid, `{"id": "evt_2"}`, false},
		{"older than 5 minutes (a replay)", signStripe(testWebhookSecret, now.Add(-6*time.Minute), body), body, false},
		{"from the future", signStripe(testWebhookSecret, now.Add(6*time.Minute), body), body, false},
		{"no header", "", body, false},
		{"garbage", "hello", body, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := verifyStripeSignature(tt.header, []byte(tt.body), testWebhookSecret, now)
			if (err == nil) != tt.ok {
				t.Errorf("err = %v, want ok = %v", err, tt.ok)
			}
		})
	}
}

// checkoutEvent is a Stripe event body for a Checkout session.
func checkoutEvent(eventType, paymentID, sessionID, paymentStatus string, amountCents int) string {
	b, _ := json.Marshal(map[string]any{
		"id": "evt_" + newUUIDv7(), "type": eventType,
		"data": map[string]any{"object": map[string]any{
			"id": sessionID, "client_reference_id": paymentID, "payment_status": paymentStatus,
			"amount_total": amountCents, "currency": "usd",
		}},
	})
	return string(b)
}

// postWebhook sends a signed (or, with sig = "", unsigned) webhook.
func postWebhook(pool *pgxpool.Pool, secret, body, sig string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("POST", "/webhooks/stripe", strings.NewReader(body))
	if sig != "" {
		req.Header.Set("Stripe-Signature", sig)
	}
	rec := httptest.NewRecorder()
	stripeWebhook(pool, secret)(rec, req)
	return rec
}

func signedWebhook(pool *pgxpool.Pool, body string) *httptest.ResponseRecorder {
	return postWebhook(pool, testWebhookSecret, body, signStripe(testWebhookSecret, time.Now(), body))
}

// pendingStripePayment records a pending $50 (+$5) payment with a Checkout
// session, as POST /payments/checkout would, and returns its ID and session.
func pendingStripePayment(t *testing.T, pool *pgxpool.Pool, s student) (string, string) {
	t.Helper()
	p, err := recordPendingPayment(context.Background(), pool, s.accountID, newUUIDv7(), pack50)
	if err != nil {
		t.Fatal(err)
	}
	session := "cs_test_" + p.id
	if _, err := pool.Exec(context.Background(), `UPDATE payment SET provider_ref = $2 WHERE id = $1`, p.id, session); err != nil {
		t.Fatal(err)
	}
	return p.id, session
}

func outcomeOf(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var body struct{ Outcome string }
	json.Unmarshal(rec.Body.Bytes(), &body)
	return body.Outcome
}

func paymentStatus(t *testing.T, pool *pgxpool.Pool, id string) string {
	t.Helper()
	var s string
	if err := pool.QueryRow(context.Background(), `SELECT status FROM payment WHERE id = $1`, id).Scan(&s); err != nil {
		t.Fatal(err)
	}
	return s
}

func TestStripeWebhook(t *testing.T) {
	pool := migratedSchemaPool(t, 4)
	ctx := context.Background()

	t.Run("a paid checkout credits the payment and the bonus, once", func(t *testing.T) {
		s := newStudent(t, pool, 0, false)
		id, session := pendingStripePayment(t, pool, s)
		body := checkoutEvent("checkout.session.completed", id, session, "paid", 5000)

		rec := signedWebhook(pool, body)
		if rec.Code != 200 || outcomeOf(t, rec) != "credited" {
			t.Fatalf("%d %s, want 200 credited", rec.Code, rec.Body)
		}
		if st := paymentStatus(t, pool, id); st != "succeeded" {
			t.Errorf("payment %s, want succeeded", st)
		}
		if got := balance(t, pool, s.accountID); got != 5500 {
			t.Errorf("balance %d, want 5500 ($50 paid + $5 bonus)", got)
		}
		if n := count(t, pool, `SELECT count(*) FROM credit_ledger WHERE payment_id = $1 AND
			((entry_type = 'purchase' AND amount_cents = 5000) OR (entry_type = 'bonus' AND amount_cents = 500))`, id); n != 2 {
			t.Errorf("%d ledger entries, want a purchase of 5000 and a bonus of 500", n)
		}

		// Stripe delivers it again (it may, even after a 200): nothing more happens.
		again := signedWebhook(pool, body)
		if again.Code != 200 || outcomeOf(t, again) != "already_credited" || balance(t, pool, s.accountID) != 5500 {
			t.Errorf("redelivery: %d %s, balance %d", again.Code, again.Body, balance(t, pool, s.accountID))
		}

		// The worker's receipt email.
		ev := eventFor(t, pool, id, "payment_succeeded")
		m := &recordingMailer{}
		if err := notifyHandler(pool, m.send, "https://hablemos.live")(ctx, ev); err != nil {
			t.Fatal(err)
		}
		if len(m.sent) != 1 || m.sent[0].Subject != "Credit added: $55.00 · Crédito añadido: 55,00 US$" ||
			!strings.Contains(m.sent[0].Text, "($50.00 paid + $5.00 bonus)") || !strings.Contains(m.sent[0].Text, "Your credit now: $55.00") {
			t.Errorf("receipt %+v", m.sent)
		}
	})

	t.Run("refusals and non-events", func(t *testing.T) {
		s := newStudent(t, pool, 0, false)
		id, session := pendingStripePayment(t, pool, s)
		paid := checkoutEvent("checkout.session.completed", id, session, "paid", 5000)

		tests := []struct {
			name, body, sig string
			secret          string
			wantStatus      int
			wantOutcome     string
		}{
			{"unsigned", paid, "", testWebhookSecret, 400, ""},
			{"signed with another secret", paid, signStripe("whsec_other", time.Now(), paid), testWebhookSecret, 400, ""},
			{"no webhook secret configured", paid, signStripe(testWebhookSecret, time.Now(), paid), "", 503, ""},
			{"completed but not paid yet (a delayed payment method)",
				checkoutEvent("checkout.session.completed", id, session, "unpaid", 5000), "", testWebhookSecret, 200, "ignored"},
			{"Stripe's amount isn't ours",
				checkoutEvent("checkout.session.completed", id, session, "paid", 500), "", testWebhookSecret, 200, "amount_mismatch"},
			{"another session than ours",
				checkoutEvent("checkout.session.completed", id, "cs_test_other", "paid", 5000), "", testWebhookSecret, 200, "amount_mismatch"},
			{"a payment we don't know",
				checkoutEvent("checkout.session.completed", newUUIDv7(), session, "paid", 5000), "", testWebhookSecret, 200, "unknown_payment"},
			{"an event type we don't act on",
				checkoutEvent("customer.created", id, session, "paid", 5000), "", testWebhookSecret, 200, "ignored"},
		}
		for _, tt := range tests {
			sig := tt.sig
			if sig == "" && tt.wantStatus != 400 {
				sig = signStripe(testWebhookSecret, time.Now(), tt.body)
			}
			rec := postWebhook(pool, tt.secret, tt.body, sig)
			if rec.Code != tt.wantStatus || outcomeOf(t, rec) != tt.wantOutcome {
				t.Errorf("%s: %d %s, want %d %q", tt.name, rec.Code, rec.Body, tt.wantStatus, tt.wantOutcome)
			}
		}
		if st := paymentStatus(t, pool, id); st != "pending" || balance(t, pool, s.accountID) != 0 {
			t.Errorf("after all that: payment %s, balance %d; want still pending, 0", st, balance(t, pool, s.accountID))
		}
	})

	t.Run("an expired checkout fails the payment; a paid one is never failed", func(t *testing.T) {
		s := newStudent(t, pool, 0, false)
		id, session := pendingStripePayment(t, pool, s)
		if rec := signedWebhook(pool, checkoutEvent("checkout.session.expired", id, session, "unpaid", 5000)); outcomeOf(t, rec) != "failed" {
			t.Errorf("expired: %s", rec.Body)
		}
		if st := paymentStatus(t, pool, id); st != "failed" {
			t.Errorf("payment %s, want failed", st)
		}

		paidID, paidSession := pendingStripePayment(t, pool, s)
		signedWebhook(pool, checkoutEvent("checkout.session.completed", paidID, paidSession, "paid", 5000))
		if rec := signedWebhook(pool, checkoutEvent("checkout.session.expired", paidID, paidSession, "unpaid", 5000)); outcomeOf(t, rec) != "not_pending" {
			t.Errorf("expired after paying: %s", rec.Body)
		}
		if st := paymentStatus(t, pool, paidID); st != "succeeded" {
			t.Errorf("paid payment is now %s, want still succeeded", st)
		}
	})
}

// The same event delivered several times at once credits once. A gate holds
// the payment row locked until every delivery is waiting (see
// TestCancelBookingRace for why); without creditPayment's FOR UPDATE, each
// delivery would read "pending" and credit.
func TestStripeWebhookRace(t *testing.T) {
	pool := migratedSchemaPool(t, 10)
	ctx := context.Background()
	s := newStudent(t, pool, 0, false)
	id, session := pendingStripePayment(t, pool, s)
	body := checkoutEvent("checkout.session.completed", id, session, "paid", 5000)

	gate, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer gate.Rollback(ctx)
	if _, err := gate.Exec(ctx, `SELECT 1 FROM payment WHERE id = $1 FOR UPDATE`, id); err != nil {
		t.Fatal(err)
	}
	const deliveries = 4
	codes := make(chan int, deliveries)
	for range deliveries {
		go func() { codes <- signedWebhook(pool, body).Code }()
	}
	waitForLockWaiters(t, pool, deliveries)
	gate.Rollback(ctx)
	for range deliveries {
		if c := <-codes; c != 200 {
			t.Errorf("a delivery answered %d, want 200", c)
		}
	}
	if n := count(t, pool, `SELECT count(*) FROM credit_ledger WHERE payment_id = $1 AND entry_type = 'purchase'`, id); n != 1 {
		t.Errorf("%d purchase entries, want exactly 1", n)
	}
	if got := balance(t, pool, s.accountID); got != 5500 {
		t.Errorf("balance %d, want 5500", got)
	}
}

// GET /payments/{id}: the page polls it after coming back from Stripe.
func TestGetPayment(t *testing.T) {
	pool := migratedSchemaPool(t, 4)
	s := newStudent(t, pool, 0, false)
	id, session := pendingStripePayment(t, pool, s)
	get := func(cookie *http.Cookie, pid string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("GET", "/payments/"+pid, nil)
		req.SetPathValue("id", pid)
		if cookie != nil {
			req.AddCookie(cookie)
		}
		rec := httptest.NewRecorder()
		requireSignIn(pool, getPayment(pool))(rec, req)
		return rec
	}
	type answer struct {
		Status       string `json:"status"`
		CreditCents  int    `json:"credit_cents"`
		BalanceCents int    `json:"balance_cents"`
	}
	var p answer
	decode := func(rec *httptest.ResponseRecorder) {
		p = answer{}
		json.Unmarshal(rec.Body.Bytes(), &p)
	}

	decode(get(s.cookie, id))
	if p.Status != "pending" || p.CreditCents != 5500 || p.BalanceCents != 0 {
		t.Errorf("before the webhook: %+v; want pending, credit 5500, balance 0", p)
	}
	signedWebhook(pool, checkoutEvent("checkout.session.completed", id, session, "paid", 5000))
	decode(get(s.cookie, id))
	if p.Status != "succeeded" || p.BalanceCents != 5500 {
		t.Errorf("after the webhook: %+v; want succeeded, balance 5500", p)
	}
	if rec := get(newStudent(t, pool, 0, false).cookie, id); rec.Code != 404 {
		t.Errorf("someone else's payment: %d, want 404", rec.Code)
	}
	if rec := get(nil, id); rec.Code != 401 {
		t.Errorf("signed out: %d, want 401", rec.Code)
	}
}
