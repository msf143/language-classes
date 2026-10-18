package main

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/mail"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	loginTokenTTL    = 15 * time.Minute // how long an emailed link works
	loginLinksPerTTL = 5                // max links per email address within that window
)

// loginSender delivers a login link. In dev it writes the link to the log;
// in production it will call the email provider.
type loginSender func(r *http.Request, email, link string) error

// logLoginLink is the dev sender: the link goes to the log instead of an inbox.
func logLoginLink(r *http.Request, email, link string) error {
	logFrom(r).Info("login link (dev only, not emailed)", "email", email, "link", link)
	return nil
}

// noLoginSender is used when no email provider is configured outside dev,
// so a misconfigured server fails loudly instead of logging live links.
func noLoginSender(r *http.Request, email, link string) error {
	return errors.New("no email sender configured (set APP_ENV=dev to log links instead)")
}

// newSecret returns 32 random bytes as URL-safe text (43 characters).
// This raw value only ever goes into the email link or the cookie.
func newSecret() string {
	b := make([]byte, 32)
	rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

// hashSecret is what the database stores and looks up instead of the secret.
func hashSecret(secret string) []byte {
	sum := sha256.Sum256([]byte(secret)) // a [32]byte array
	return sum[:]                        // as a slice, which pgx sends as bytea
}

// normalizeEmail trims and lower-cases an address and rejects anything that
// isn't a plain address (no "Name <a@b>" forms, no spaces, sane length).
func normalizeEmail(s string) (string, bool) {
	s = strings.ToLower(strings.TrimSpace(s))
	if len(s) > 254 {
		return "", false
	}
	addr, err := mail.ParseAddress(s)
	if err != nil || addr.Address != s || !strings.Contains(s[strings.LastIndex(s, "@"):], ".") {
		return "", false
	}
	return s, true
}

// requestLoginLink handles POST /auth/login with body {"email": "..."}.
//
// It answers the same way whether or not the address has an account, so the
// form can't be used to discover who is a student. The link points at the
// sign-in page with the token after "#": browsers never send that part to a
// server, so the token stays out of server and proxy logs. The page then
// POSTs it to /auth/verify when the person clicks "Sign in".
func requestLoginLink(pool *pgxpool.Pool, send loginSender, baseURL string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Email string `json:"email"`
		}
		r.Body = http.MaxBytesReader(w, r.Body, 1<<10) // 1 KB is plenty for an email address
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeError(w, r, http.StatusBadRequest, `body must be JSON like {"email": "you@example.com"}`)
			return
		}
		email, ok := normalizeEmail(body.Email)
		if !ok {
			writeError(w, r, http.StatusBadRequest, "please enter a valid email address")
			return
		}
		ctx := r.Context()

		var recent int
		err := pool.QueryRow(ctx, `
			SELECT count(*) FROM login_token
			WHERE lower(email) = $1 AND created_at > now() - $2::int * interval '1 second'`,
			email, int(loginTokenTTL.Seconds())).Scan(&recent)
		if err != nil {
			logFrom(r).Error("login: rate-limit query failed", "err", err)
			writeError(w, r, http.StatusInternalServerError, "could not send a login link")
			return
		}
		if recent >= loginLinksPerTTL {
			writeError(w, r, http.StatusTooManyRequests, "too many login links requested; try again in a few minutes")
			return
		}

		// A disabled account gets the same answer, but no link.
		var status string
		err = pool.QueryRow(ctx, `SELECT status FROM account WHERE lower(email) = $1`, email).Scan(&status)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			logFrom(r).Error("login: account lookup failed", "err", err)
			writeError(w, r, http.StatusInternalServerError, "could not send a login link")
			return
		}
		if status != "disabled" {
			secret := newSecret()
			_, err = pool.Exec(ctx, `
				INSERT INTO login_token (id, email, token_hash, expires_at)
				VALUES ($1, $2, $3, now() + $4::int * interval '1 second')`,
				newUUIDv7(), email, hashSecret(secret), int(loginTokenTTL.Seconds()))
			if err != nil {
				logFrom(r).Error("login: insert token failed", "err", err)
				writeError(w, r, http.StatusInternalServerError, "could not send a login link")
				return
			}
			if err := send(r, email, baseURL+"/signin.html#token="+secret); err != nil {
				logFrom(r).Error("login: sending link failed", "err", err)
				writeError(w, r, http.StatusInternalServerError, "could not send a login link")
				return
			}
		}

		writeJSON(w, http.StatusOK, map[string]string{"message": "check your email for a sign-in link"})
	}
}
