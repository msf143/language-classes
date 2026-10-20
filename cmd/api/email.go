package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

const (
	resendEndpoint = "https://api.resend.com/emails"
	loginEmailFrom = "Hablemos <login@hablemos.live>" // the domain verified in Resend
)

// resendSender returns a loginSender that emails the link through Resend's
// REST API: one HTTPS POST with a JSON body, so no SDK is needed. endpoint is
// a parameter so tests can point it at a fake server. subjectPrefix goes in
// front of the subject ("[TEST] " on staging, "" on production).
func resendSender(apiKey, endpoint, subjectPrefix string) loginSender {
	// Never wait forever on an outside service: the student is waiting too.
	client := &http.Client{Timeout: 10 * time.Second}

	return func(r *http.Request, email, link string) error {
		minutes := int(loginTokenTTL.Minutes())
		body, err := json.Marshal(map[string]any{
			"from":    loginEmailFrom,
			"to":      []string{email},
			"subject": subjectPrefix + "Your sign-in link · Tu enlace para entrar",
			// Plain text, both languages: the request doesn't say which one the
			// student reads, and every mail program shows plain text and makes
			// the link clickable.
			"text": fmt.Sprintf(`Hello! Use this link to sign in to Hablemos. It works once, for %[1]d minutes.
¡Hola! Usa este enlace para entrar a Hablemos. Funciona una sola vez, durante %[1]d minutos.

%[2]s

If you didn't ask for this, you can ignore this email.
Si no lo pediste, puedes ignorar este correo.
`, minutes, link),
		})
		if err != nil {
			return err
		}

		req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, endpoint, bytes.NewReader(body))
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", "Bearer "+apiKey)
		req.Header.Set("Content-Type", "application/json")

		resp, err := client.Do(req)
		if err != nil {
			return fmt.Errorf("resend: %w", err)
		}
		defer resp.Body.Close()

		// Read a bounded amount: enough for Resend's small JSON answer or error.
		answer, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		if resp.StatusCode != http.StatusOK {
			// Resend's error body explains the cause (bad key, unverified domain...).
			// It never contains our key, so it's safe to log.
			return fmt.Errorf("resend: status %d: %s", resp.StatusCode, answer)
		}

		var sent struct {
			ID string `json:"id"`
		}
		_ = json.Unmarshal(answer, &sent)
		// Log Resend's message ID (to look it up in their dashboard), never the link.
		logFrom(r).Info("login link emailed", "resend_id", sent.ID)
		return nil
	}
}
