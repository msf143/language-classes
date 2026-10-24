package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"
)

const (
	resendEndpoint   = "https://api.resend.com/emails"
	loginEmailFrom   = "Hablemos <login@hablemos.live>"   // the domain verified in Resend
	classesEmailFrom = "Hablemos <classes@hablemos.live>" // booking confirmations, cancellations, reminders
	// Where replies go: the From addresses above only send. hola@ is forwarded
	// to a person by Cloudflare Email Routing (infra/inbox.tf).
	replyToAddress = "Hablemos <hola@hablemos.live>"
)

// email is one message to send. Plain text, both languages, everywhere: every
// mail program shows plain text and makes links clickable.
type email struct {
	From, To, Subject, Text string
	// IdempotencyKey makes a resend of the same message a no-op at Resend (for
	// 24 hours). The worker uses the outbox event's ID, so a retried event
	// doesn't email twice. Empty = no key.
	IdempotencyKey string
}

// mailer sends one email and returns the provider's message ID.
type mailer func(ctx context.Context, m email) (id string, err error)

// resendMailer sends through Resend's REST API: one HTTPS POST with a JSON
// body, so no SDK is needed. endpoint is a parameter so tests can point it at
// a fake server. subjectPrefix goes in front of every subject ("[TEST] " on
// staging, "" on production).
func resendMailer(apiKey, endpoint, subjectPrefix string) mailer {
	// Never wait forever on an outside service.
	client := &http.Client{Timeout: 10 * time.Second}

	return func(ctx context.Context, m email) (string, error) {
		body, err := json.Marshal(map[string]any{
			"from":     m.From,
			"to":       []string{m.To},
			"reply_to": replyToAddress, // every email: pressing Reply reaches a person
			"subject":  subjectPrefix + m.Subject,
			"text":     m.Text,
		})
		if err != nil {
			return "", err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
		if err != nil {
			return "", err
		}
		req.Header.Set("Authorization", "Bearer "+apiKey)
		req.Header.Set("Content-Type", "application/json")
		if m.IdempotencyKey != "" {
			req.Header.Set("Idempotency-Key", m.IdempotencyKey)
		}

		resp, err := client.Do(req)
		if err != nil {
			return "", fmt.Errorf("resend: %w", err)
		}
		defer resp.Body.Close()

		// Read a bounded amount: enough for Resend's small JSON answer or error.
		answer, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		if resp.StatusCode != http.StatusOK {
			// Resend's error body explains the cause (bad key, unverified domain...).
			// It never contains our key, so it's safe to log.
			return "", fmt.Errorf("resend: status %d: %s", resp.StatusCode, answer)
		}
		var sent struct {
			ID string `json:"id"`
		}
		_ = json.Unmarshal(answer, &sent)
		return sent.ID, nil
	}
}

// logMailer is the dev mailer: it writes the email to the log instead of
// sending it, so a dev setup never emails a real address.
func logMailer(_ context.Context, m email) (string, error) {
	slog.Info("email (dev only, not sent)", "to", m.To, "subject", m.Subject, "text", m.Text)
	return "dev-not-sent", nil
}

// resendSender returns a loginSender that emails the sign-in link through
// Resend (see resendMailer).
func resendSender(apiKey, endpoint, subjectPrefix string) loginSender {
	send := resendMailer(apiKey, endpoint, subjectPrefix)
	return func(r *http.Request, to, link string) error {
		minutes := int(loginTokenTTL.Minutes())
		id, err := send(r.Context(), email{
			From:    loginEmailFrom,
			To:      to,
			Subject: "Your sign-in link · Tu enlace para entrar",
			Text: fmt.Sprintf(`Hello! Use this link to sign in to Hablemos. It works once, for %[1]d minutes.
¡Hola! Usa este enlace para entrar a Hablemos. Funciona una sola vez, durante %[1]d minutos.

%[2]s

If you didn't ask for this, you can ignore this email.
Si no lo pediste, puedes ignorar este correo.
`, minutes, link),
		})
		if err != nil {
			return err
		}
		// Log Resend's message ID (to look it up in their dashboard), never the link.
		logFrom(r).Info("login link emailed", "resend_id", id)
		return nil
	}
}
