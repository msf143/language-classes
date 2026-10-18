package main

import (
	"context"
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
	loginTokenTTL    = 15 * time.Minute    // how long an emailed link works
	loginLinksPerTTL = 5                   // max links per email address within that window
	webSessionTTL    = 30 * 24 * time.Hour // how long a browser stays signed in
	sessionCookie    = "session"           // cookie holding the web_session secret
)

// account is the signed-in person, as the API returns it (GET /me, POST /auth/verify).
type account struct {
	ID                string `json:"id"`
	Email             string `json:"email"`
	DisplayName       string `json:"display_name"`
	PreferredLanguage string `json:"preferred_language"`
	Timezone          string `json:"timezone"`
}

// accountColumns selects an account (aliased "a") in the field order of account.
const accountColumns = `a.id::text, a.email, a.display_name, a.preferred_language, a.timezone`

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

// verifyLoginToken handles POST /auth/verify with body {"token": "..."}.
// The sign-in page sends the token from the link when the person clicks
// "Sign in".
//
// In one transaction it uses up the token, finds or creates the account, and
// opens a web_session. Then it sets the session cookie. Expired, used and
// unknown tokens all get the same answer.
func verifyLoginToken(pool *pgxpool.Pool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Token string `json:"token"`
		}
		r.Body = http.MaxBytesReader(w, r.Body, 1<<10)
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Token == "" || len(body.Token) > 100 {
			writeError(w, r, http.StatusBadRequest, `body must be JSON like {"token": "..."}`)
			return
		}
		ctx := r.Context()

		tx, err := pool.Begin(ctx)
		if err != nil {
			logFrom(r).Error("verify: begin failed", "err", err)
			writeError(w, r, http.StatusInternalServerError, "could not sign you in")
			return
		}
		// Rollback undoes everything if we return early. After Commit it does nothing.
		defer tx.Rollback(ctx)

		// Use up the token: a conditional UPDATE, so only one request can win
		// even if the button is double-clicked (the second finds used_at set).
		var email string
		err = tx.QueryRow(ctx, `
			UPDATE login_token SET used_at = now()
			WHERE token_hash = $1 AND used_at IS NULL AND expires_at > now()
			RETURNING email`,
			hashSecret(body.Token)).Scan(&email)
		if errors.Is(err, pgx.ErrNoRows) {
			writeError(w, r, http.StatusBadRequest, "this sign-in link is invalid, used, or expired; please request a new one")
			return
		}
		if err != nil {
			logFrom(r).Error("verify: claim token failed", "err", err)
			writeError(w, r, http.StatusInternalServerError, "could not sign you in")
			return
		}

		// First sign-in creates the account. ON CONFLICT targets the unique
		// index on lower(email), so an existing account is left alone. The
		// display name starts as the part before "@"; the student can change it.
		_, err = tx.Exec(ctx, `
			INSERT INTO account (id, email, display_name) VALUES ($1, $2, $3)
			ON CONFLICT ((lower(email))) DO NOTHING`,
			newUUIDv7(), email, email[:strings.Index(email, "@")])
		if err != nil {
			logFrom(r).Error("verify: create account failed", "err", err)
			writeError(w, r, http.StatusInternalServerError, "could not sign you in")
			return
		}
		var acct account
		var status string
		err = tx.QueryRow(ctx, `
			SELECT `+accountColumns+`, a.status FROM account a WHERE lower(a.email) = $1`,
			email).Scan(&acct.ID, &acct.Email, &acct.DisplayName, &acct.PreferredLanguage, &acct.Timezone, &status)
		if err != nil {
			logFrom(r).Error("verify: load account failed", "err", err)
			writeError(w, r, http.StatusInternalServerError, "could not sign you in")
			return
		}
		if status == "disabled" {
			writeError(w, r, http.StatusForbidden, "this account is disabled")
			return
		}

		secret := newSecret()
		_, err = tx.Exec(ctx, `
			INSERT INTO web_session (id, account_id, token_hash, expires_at)
			VALUES ($1, $2, $3, now() + $4::int * interval '1 second')`,
			newUUIDv7(), acct.ID, hashSecret(secret), int(webSessionTTL.Seconds()))
		if err != nil {
			logFrom(r).Error("verify: create web session failed", "err", err)
			writeError(w, r, http.StatusInternalServerError, "could not sign you in")
			return
		}
		if err := tx.Commit(ctx); err != nil {
			logFrom(r).Error("verify: commit failed", "err", err)
			writeError(w, r, http.StatusInternalServerError, "could not sign you in")
			return
		}

		// HttpOnly: page JavaScript can't read it (limits what an XSS bug could steal).
		// Secure: only sent over HTTPS (browsers treat http://localhost as secure too).
		// SameSite=Lax: not sent on form posts or fetches from other sites (CSRF).
		http.SetCookie(w, &http.Cookie{
			Name:     sessionCookie,
			Value:    secret,
			Path:     "/",
			MaxAge:   int(webSessionTTL.Seconds()),
			HttpOnly: true,
			Secure:   true,
			SameSite: http.SameSiteLaxMode,
		})
		writeJSON(w, http.StatusOK, map[string]any{"account": acct})
	}
}

// accountKey is where signedIn stores the *account in the request context.
const accountKey ctxKey = "account"

// signedIn wraps a handler that works with or without a signed-in person.
// If the session cookie matches a live web_session of an active account, the
// account is put in the request context; read it with accountFrom. A missing,
// expired, revoked or unknown cookie just means "signed out", not an error.
//
// It wraps single routes rather than the whole server so that requests for
// static files (which also carry the cookie) don't each cost a query.
func signedIn(pool *pgxpool.Pool, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c, err := r.Cookie(sessionCookie)
		if err != nil || c.Value == "" || len(c.Value) > 100 {
			next(w, r)
			return
		}
		var a account
		err = pool.QueryRow(r.Context(), `
			SELECT `+accountColumns+`
			FROM web_session s JOIN account a ON a.id = s.account_id
			WHERE s.token_hash = $1 AND s.revoked_at IS NULL AND s.expires_at > now()
			  AND a.status = 'active'`,
			hashSecret(c.Value)).Scan(&a.ID, &a.Email, &a.DisplayName, &a.PreferredLanguage, &a.Timezone)
		if errors.Is(err, pgx.ErrNoRows) {
			next(w, r)
			return
		}
		if err != nil {
			logFrom(r).Error("auth: session lookup failed", "err", err)
			writeError(w, r, http.StatusInternalServerError, "could not check your sign-in")
			return
		}
		next(w, r.WithContext(context.WithValue(r.Context(), accountKey, &a)))
	}
}

// requireSignIn is signedIn for routes that only make sense signed in:
// without an account it answers 401 and never calls next.
func requireSignIn(pool *pgxpool.Pool, next http.HandlerFunc) http.HandlerFunc {
	return signedIn(pool, func(w http.ResponseWriter, r *http.Request) {
		if _, ok := accountFrom(r); !ok {
			writeError(w, r, http.StatusUnauthorized, "please sign in")
			return
		}
		next(w, r)
	})
}

// accountFrom returns the account signedIn found, if any.
func accountFrom(r *http.Request) (*account, bool) {
	a, ok := r.Context().Value(accountKey).(*account)
	return a, ok
}

// getMe handles GET /me (wrapped in requireSignIn).
func getMe(w http.ResponseWriter, r *http.Request) {
	a, _ := accountFrom(r)
	writeJSON(w, http.StatusOK, map[string]any{"account": a})
}

// logout handles POST /auth/logout. It revokes this browser's web_session
// (the row stays, with revoked_at set) and tells the browser to delete the
// cookie. It succeeds even if nobody was signed in, so it's safe to repeat.
func logout(pool *pgxpool.Pool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if c, err := r.Cookie(sessionCookie); err == nil && c.Value != "" {
			_, err := pool.Exec(r.Context(), `
				UPDATE web_session SET revoked_at = now()
				WHERE token_hash = $1 AND revoked_at IS NULL`,
				hashSecret(c.Value))
			if err != nil {
				logFrom(r).Error("logout: revoke failed", "err", err)
				writeError(w, r, http.StatusInternalServerError, "could not sign you out")
				return
			}
		}
		// Same name, path and flags as when it was set; MaxAge < 0 means "delete now".
		http.SetCookie(w, &http.Cookie{
			Name:     sessionCookie,
			Value:    "",
			Path:     "/",
			MaxAge:   -1,
			HttpOnly: true,
			Secure:   true,
			SameSite: http.SameSiteLaxMode,
		})
		writeJSON(w, http.StatusOK, map[string]string{"message": "signed out"})
	}
}
