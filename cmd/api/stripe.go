package main

// A minimal Stripe client: just the calls the app makes, over Stripe's REST
// API with net/http (no SDK, like the Resend client). Stripe takes form-encoded
// request bodies and answers JSON.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const stripeAPI = "https://api.stripe.com"

type stripeClient struct {
	secretKey string // sk_test_… on dev and staging, sk_live_… on production
	base      string // stripeAPI, or a fake server in tests
	http      *http.Client
}

// newStripe returns a client, or nil when no key is configured (then buying
// credit answers "not available yet" instead of failing).
func newStripe(secretKey, base string) *stripeClient {
	if secretKey == "" {
		return nil
	}
	return &stripeClient{secretKey: secretKey, base: base, http: &http.Client{Timeout: 15 * time.Second}}
}

// checkoutRequest is what a Checkout session needs from us.
type checkoutRequest struct {
	PaymentID   string // ours; Stripe hands it back in the webhook (client_reference_id)
	AmountCents int
	Currency    string // "USD"
	ProductName string // what the student sees they're paying for
	Email       string // pre-fills Stripe's form and receipt
	SuccessURL  string // where Stripe sends them after paying
	CancelURL   string // ... or after backing out
}

// createCheckoutSession asks Stripe for a hosted payment page and returns the
// session's ID and URL. The Idempotency-Key is derived from our payment ID, so
// asking twice for the same payment returns the same session (Stripe keeps keys
// for 24 hours).
func (s *stripeClient) createCheckoutSession(ctx context.Context, c checkoutRequest) (id, pageURL string, err error) {
	form := url.Values{
		"mode":                 {"payment"},
		"client_reference_id":  {c.PaymentID},
		"metadata[payment_id]": {c.PaymentID},
		"payment_intent_data[metadata][payment_id]": {c.PaymentID},
		"customer_email":                                {c.Email},
		"success_url":                                   {c.SuccessURL},
		"cancel_url":                                    {c.CancelURL},
		"line_items[0][quantity]":                       {"1"},
		"line_items[0][price_data][currency]":           {strings.ToLower(c.Currency)},
		"line_items[0][price_data][unit_amount]":        {strconv.Itoa(c.AmountCents)},
		"line_items[0][price_data][product_data][name]": {c.ProductName},
	}
	var session struct {
		ID  string `json:"id"`
		URL string `json:"url"`
	}
	err = s.post(ctx, "/v1/checkout/sessions", form, "checkout-"+c.PaymentID, &session)
	return session.ID, session.URL, err
}

// post sends a form to Stripe and decodes the JSON answer into out.
func (s *stripeClient) post(ctx context.Context, path string, form url.Values, idempotencyKey string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.base+path, strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.SetBasicAuth(s.secretKey, "") // Stripe's documented auth: the key as the user name
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if idempotencyKey != "" {
		req.Header.Set("Idempotency-Key", idempotencyKey)
	}
	resp, err := s.http.Do(req)
	if err != nil {
		return fmt.Errorf("stripe: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode != http.StatusOK {
		// Stripe's error JSON says what's wrong; it never contains our key.
		return fmt.Errorf("stripe: status %d: %s", resp.StatusCode, body)
	}
	return json.Unmarshal(body, out)
}
