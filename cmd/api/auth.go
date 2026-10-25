package main

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
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
	loginCodeTries   = 5                   // guesses per emailed code (right or wrong); then it stops working
	loginCodeDayCap  = 20                  // guesses per email address per day, across all its codes
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
	IsTeacher         bool   `json:"is_teacher"` // derived: has an active or pending teacher profile (may use the teacher page)
	IsAdmin           bool   `json:"is_admin"`   // account.is_admin (set by `api admin`)
	Welcomed          bool   `json:"welcomed"`   // has seen the one-time welcome (account.welcomed_at set)
}

// accountColumns selects an account (aliased "a") in the field order of account.
// is_teacher is computed on every read rather than stored, so it can't drift
// from the teacher table.
const accountColumns = `a.id::text, a.email, a.display_name, a.preferred_language, a.timezone,
	EXISTS (SELECT 1 FROM teacher t WHERE t.account_id = a.id AND t.status IN ('active', 'pending')), a.is_admin,
	a.welcomed_at IS NOT NULL`

// loginSender delivers a login link and its 6-digit code. In dev it writes
// them to the log; in production it calls the email provider.
type loginSender func(r *http.Request, email, link, code string) error

// logLoginLink is the dev sender: the link and code go to the log instead of an inbox.
func logLoginLink(r *http.Request, email, link, code string) error {
	logFrom(r).Info("login link (dev only, not emailed)", "email", email, "link", link, "code", code)
	return nil
}

// noLoginSender is used when no email provider is configured outside dev,
// so a misconfigured server fails loudly instead of logging live links.
func noLoginSender(r *http.Request, email, link, code string) error {
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

// newLoginCode returns 6 random digits ("042917"). crypto/rand, like every
// secret here: math/rand would be predictable.
func newLoginCode() string {
	n, _ := rand.Int(rand.Reader, big.NewInt(1_000_000))
	return fmt.Sprintf("%06d", n)
}

// loginCodeHash is what login_token.code_hash stores for a code. The row's ID
// goes in too, so the same code in two emails never has the same hash.
func loginCodeHash(tokenID, code string) []byte { return hashSecret(tokenID + ":" + code) }

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
		switch err := issueLoginLink(r, pool, send, baseURL, email); {
		case errors.Is(err, errTooManyLinks):
			writeError(w, r, http.StatusTooManyRequests, "too many login links requested; try again in a few minutes")
			return
		case err != nil:
			logFrom(r).Error("login: issuing a link failed", "err", err)
			writeError(w, r, http.StatusInternalServerError, "could not send a login link")
			return
		}

		writeJSON(w, http.StatusOK, map[string]any{
			"message":            "check your email for a sign-in link",
			"expires_in_minutes": int(loginTokenTTL.Minutes()), // so the UI doesn't hard-code it
		})
	}
}

var errTooManyLinks = errors.New("too many login links requested")

// issueLoginLink sends email a fresh sign-in link: at most loginLinksPerTTL
// per address per loginTokenTTL (else errTooManyLinks), and none for a
// disabled account (silently: the caller answers the same either way). Only
// the secret's hash is stored. Used by POST /auth/login and POST /auth/relink.
func issueLoginLink(r *http.Request, pool *pgxpool.Pool, send loginSender, baseURL, email string) error {
	ctx := r.Context()
	var recent int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM login_token
		WHERE lower(email) = $1 AND created_at > now() - $2::int * interval '1 second'`,
		email, int(loginTokenTTL.Seconds())).Scan(&recent); err != nil {
		return err
	}
	if recent >= loginLinksPerTTL {
		return errTooManyLinks
	}
	var status string
	err := pool.QueryRow(ctx, `SELECT status FROM account WHERE lower(email) = $1`, email).Scan(&status)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	if status == "disabled" {
		return nil
	}
	id, secret, code := newUUIDv7(), newSecret(), newLoginCode()
	if _, err := pool.Exec(ctx, `
		INSERT INTO login_token (id, email, token_hash, code_hash, expires_at)
		VALUES ($1, $2, $3, $4, now() + $5::int * interval '1 second')`,
		id, email, hashSecret(secret), loginCodeHash(id, code), int(loginTokenTTL.Seconds())); err != nil {
		return err
	}
	return send(r, email, baseURL+"/signin.html#token="+secret, code)
}

// relinkLogin handles POST /auth/relink {"token": "..."}: an old sign-in link
// (used or expired) asks for a fresh one, sent to the same address, so nobody
// has to type their email again. Same rate limit as POST /auth/login. The
// answer shows the address masked ("m•••@yahoo.com"). A link the worker has
// already purged (a day after it expired) no longer knows its address:
// 404 relink_unknown, and the page shows the email box instead.
func relinkLogin(pool *pgxpool.Pool, send loginSender, baseURL string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Token string `json:"token"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<10)).Decode(&body); err != nil || body.Token == "" {
			writeError(w, r, http.StatusBadRequest, `body must be JSON like {"token": "..."}`)
			return
		}
		var email string
		err := pool.QueryRow(r.Context(), `SELECT email FROM login_token WHERE token_hash = $1`, hashSecret(body.Token)).Scan(&email)
		if errors.Is(err, pgx.ErrNoRows) {
			writeErrorCode(w, r, http.StatusNotFound, "relink_unknown", "this link is too old; enter your email instead")
			return
		}
		if err != nil {
			logFrom(r).Error("relink: lookup failed", "err", err)
			writeError(w, r, http.StatusInternalServerError, "could not send a login link")
			return
		}
		switch err := issueLoginLink(r, pool, send, baseURL, strings.ToLower(email)); {
		case errors.Is(err, errTooManyLinks):
			writeErrorCode(w, r, http.StatusTooManyRequests, "too_many_links", "too many login links requested; try again in a few minutes")
			return
		case err != nil:
			logFrom(r).Error("relink: issuing a link failed", "err", err)
			writeError(w, r, http.StatusInternalServerError, "could not send a login link")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"masked_email":       maskEmail(email),
			"expires_in_minutes": int(loginTokenTTL.Minutes()),
		})
	}
}

// maskEmail: "mike@yahoo.com" → "m•••@yahoo.com": enough to recognise your
// own address, not enough to learn someone else's.
func maskEmail(email string) string {
	at := strings.LastIndex(email, "@")
	if at < 1 {
		return "•••"
	}
	return email[:1] + "•••" + email[at:]
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
			Token    string `json:"token"`
			Timezone string `json:"timezone"` // the browser's zone, e.g. "America/New_York" (optional)
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

		acct, secret, err := signInEmail(ctx, tx, email, body.Timezone)
		if errors.Is(err, errAccountDisabled) {
			writeError(w, r, http.StatusForbidden, "this account is disabled")
			return
		}
		if err != nil {
			logFrom(r).Error("verify: sign-in failed", "err", err)
			writeError(w, r, http.StatusInternalServerError, "could not sign you in")
			return
		}
		if err := tx.Commit(ctx); err != nil {
			logFrom(r).Error("verify: commit failed", "err", err)
			writeError(w, r, http.StatusInternalServerError, "could not sign you in")
			return
		}

		setSessionCookie(w, secret)
		writeJSON(w, http.StatusOK, map[string]any{"account": acct})
	}
}

var errAccountDisabled = errors.New("account disabled")

// signInEmail is what a used-up link or code leads to, in the caller's
// transaction: find or create the account for email, save the browser's time
// zone, open a web session. Returns the account and the cookie's secret.
func signInEmail(ctx context.Context, tx pgx.Tx, email, timezone string) (account, string, error) {
	var acct account
	// First sign-in creates the account. ON CONFLICT targets the unique
	// index on lower(email), so an existing account is left alone. The
	// display name starts as the part before "@"; the student can change it.
	// RETURNING gives a row only when a row was inserted, so newAccountID
	// is set exactly when this is a brand-new account.
	name := email[:strings.Index(email, "@")]
	var newAccountID string
	err := tx.QueryRow(ctx, `
		INSERT INTO account (id, email, display_name) VALUES ($1, $2, $3)
		ON CONFLICT ((lower(email))) DO NOTHING
		RETURNING id::text`,
		newUUIDv7(), email, name).Scan(&newAccountID)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return acct, "", fmt.Errorf("create account: %w", err)
	}
	// A new account holder can book classes for themselves right away: give
	// them a learner profile (an adult; children are added later). Same
	// transaction, so there's never an account without one.
	if newAccountID != "" {
		if _, err := tx.Exec(ctx, `
			INSERT INTO learner (id, account_id, display_name, native_language, is_account_holder)
			SELECT $1, a.id, a.display_name, a.preferred_language, true FROM account a WHERE a.id = $2`,
			newUUIDv7(), newAccountID); err != nil {
			return acct, "", fmt.Errorf("create learner: %w", err)
		}
	}
	var status string
	if err := tx.QueryRow(ctx, `
		SELECT `+accountColumns+`, a.status FROM account a WHERE lower(a.email) = $1`,
		email).Scan(&acct.ID, &acct.Email, &acct.DisplayName, &acct.PreferredLanguage, &acct.Timezone, &acct.IsTeacher, &acct.IsAdmin, &acct.Welcomed, &status); err != nil {
		return acct, "", fmt.Errorf("load account: %w", err)
	}
	if status == "disabled" {
		return acct, "", errAccountDisabled
	}

	// Remember the person's time zone, so emails can show class times in it.
	// Only while the account still has the default (UTC): a zone that's set is
	// never overridden by whatever zone a browser happens to be in (a teacher
	// signing in while travelling mustn't move his availability).
	if acct.Timezone == "UTC" && validTimezone(timezone) {
		if _, err := tx.Exec(ctx, `UPDATE account SET timezone = $2 WHERE id = $1`, acct.ID, timezone); err != nil {
			return acct, "", fmt.Errorf("save timezone: %w", err)
		}
		acct.Timezone = timezone
	}

	secret, err := startWebSession(ctx, tx, acct.ID)
	if err != nil {
		return acct, "", fmt.Errorf("create web session: %w", err)
	}
	return acct, secret, nil
}

// verifyLoginCode handles POST /auth/verify-code {"email", "code", "timezone"}:
// the 6 digits from the email, typed into the page that asked for them.
//
// Guessing is what to guard against (a million codes): every try counts
// against the newest code for that address, right or wrong, and at
// loginCodeTries it stops working; loginCodeDayCap bounds the tries per
// address per day across new codes. The count goes up in one conditional
// UPDATE before the code is compared, so tries sent at the same moment can't
// all slip past the limit (each waits for the row lock, then re-checks).
// Codes: code_wrong (400), code_expired (400: none current, or the link was
// used), too_many_tries (429: ask for a new code, or use the link).
func verifyLoginCode(pool *pgxpool.Pool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Email    string `json:"email"`
			Code     string `json:"code"`
			Timezone string `json:"timezone"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<10)).Decode(&body); err != nil {
			writeError(w, r, http.StatusBadRequest, `body must be JSON like {"email": "...", "code": "123456"}`)
			return
		}
		email, ok := normalizeEmail(body.Email)
		code := strings.ReplaceAll(body.Code, " ", "") // "042 917" as some people type it
		if !ok || len(code) != 6 || strings.Trim(code, "0123456789") != "" {
			writeErrorCode(w, r, http.StatusBadRequest, "code_wrong", "enter your email and the 6 digits from the email")
			return
		}
		ctx := r.Context()
		fail := func(err error) {
			logFrom(r).Error("verify code failed", "err", err)
			writeError(w, r, http.StatusInternalServerError, "could not sign you in")
		}

		var today int
		if err := pool.QueryRow(ctx, `
			SELECT COALESCE(SUM(code_attempts), 0) FROM login_token
			WHERE lower(email) = $1 AND created_at > now() - interval '1 day'`, email).Scan(&today); err != nil {
			fail(err)
			return
		}
		if today >= loginCodeDayCap {
			writeErrorCode(w, r, http.StatusTooManyRequests, "too_many_tries", "too many tries today; use the link in the email")
			return
		}

		// Count this try against the newest current code (its own statement, so
		// it stays counted even when the code is wrong).
		var tokenID string
		var storedHash []byte
		err := pool.QueryRow(ctx, `
			UPDATE login_token SET code_attempts = code_attempts + 1
			WHERE id = (SELECT id FROM login_token
			            WHERE lower(email) = $1 AND used_at IS NULL AND expires_at > now() AND code_hash IS NOT NULL
			            ORDER BY created_at DESC LIMIT 1)
			  AND code_attempts < $2
			RETURNING id::text, code_hash`, email, loginCodeTries).Scan(&tokenID, &storedHash)
		if errors.Is(err, pgx.ErrNoRows) {
			// No current code at all, or the newest one has had its tries.
			var current bool
			if err := pool.QueryRow(ctx, `
				SELECT EXISTS (SELECT 1 FROM login_token
				               WHERE lower(email) = $1 AND used_at IS NULL AND expires_at > now() AND code_hash IS NOT NULL)`,
				email).Scan(&current); err != nil {
				fail(err)
				return
			}
			if current {
				writeErrorCode(w, r, http.StatusTooManyRequests, "too_many_tries", "too many tries; ask for a new code, or use the link in the email")
			} else {
				writeErrorCode(w, r, http.StatusBadRequest, "code_expired", "this code has expired or was used; ask for a new one")
			}
			return
		}
		if err != nil {
			fail(err)
			return
		}
		// hmac.Equal takes the same time however many bytes match, so timing
		// tells an attacker nothing.
		if !hmac.Equal(storedHash, loginCodeHash(tokenID, code)) {
			writeErrorCode(w, r, http.StatusBadRequest, "code_wrong", "that code isn't right")
			return
		}

		var acct account
		var secret string
		err = pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
			// Use up the token, as a link would (the conditional UPDATE: if the
			// link was used a moment ago, there's nothing left to use).
			if err := tx.QueryRow(ctx, `
				UPDATE login_token SET used_at = now()
				WHERE id = $1 AND used_at IS NULL AND expires_at > now()
				RETURNING email`, tokenID).Scan(&email); err != nil {
				return err
			}
			var err error
			acct, secret, err = signInEmail(ctx, tx, email, body.Timezone)
			return err
		})
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			writeErrorCode(w, r, http.StatusBadRequest, "code_expired", "this code has expired or was used; ask for a new one")
		case errors.Is(err, errAccountDisabled):
			writeError(w, r, http.StatusForbidden, "this account is disabled")
		case err != nil:
			fail(err)
		default:
			setSessionCookie(w, secret)
			writeJSON(w, http.StatusOK, map[string]any{"account": acct})
		}
	}
}

// startWebSession records a signed-in browser for the account (only the
// secret's hash is stored) and returns the secret for the cookie. Used by
// sign-in and by accepting a teacher invitation.
func startWebSession(ctx context.Context, tx pgx.Tx, accountID string) (string, error) {
	secret := newSecret()
	_, err := tx.Exec(ctx, `
		INSERT INTO web_session (id, account_id, token_hash, expires_at)
		VALUES ($1, $2, $3, now() + $4::int * interval '1 second')`,
		newUUIDv7(), accountID, hashSecret(secret), int(webSessionTTL.Seconds()))
	return secret, err
}

// setSessionCookie sends the browser its session secret.
// HttpOnly: page JavaScript can't read it (limits what an XSS bug could steal).
// Secure: only sent over HTTPS (browsers treat http://localhost as secure too).
// SameSite=Lax: not sent on form posts or fetches from other sites (CSRF).
func setSessionCookie(w http.ResponseWriter, secret string) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    secret,
		Path:     "/",
		MaxAge:   int(webSessionTTL.Seconds()),
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteLaxMode,
	})
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
			hashSecret(c.Value)).Scan(&a.ID, &a.Email, &a.DisplayName, &a.PreferredLanguage, &a.Timezone, &a.IsTeacher, &a.IsAdmin, &a.Welcomed)
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

// learnerDTO is one of an account's learner profiles (who can attend a class).
type learnerDTO struct {
	ID              string `json:"id"`
	DisplayName     string `json:"display_name"`
	IsMinor         bool   `json:"is_minor"`
	NativeLanguage  string `json:"native_language"`   // "en" or "es"
	IsAccountHolder bool   `json:"is_account_holder"` // the account holder themselves ("you"); can't be removed
}

// getMe handles GET /me (wrapped in requireSignIn): the signed-in account,
// its credit balance, and its active learners (what the Book dialog needs).
func getMe(pool *pgxpool.Pool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		a, _ := accountFrom(r)
		ctx := r.Context()

		// Balance is derived from the ledger, never stored (see account_balance).
		var balance int
		err := pool.QueryRow(ctx, `
			SELECT COALESCE(SUM(amount_cents), 0) FROM credit_ledger
			WHERE account_id = $1 AND currency = 'USD'`, a.ID).Scan(&balance)
		if err != nil {
			logFrom(r).Error("me: balance query failed", "err", err)
			writeError(w, r, http.StatusInternalServerError, "could not load your account")
			return
		}

		rows, err := pool.Query(ctx, `
			SELECT id::text, display_name, is_minor, native_language, is_account_holder FROM learner
			WHERE account_id = $1 AND status = 'active'
			ORDER BY created_at`, a.ID)
		if err != nil {
			logFrom(r).Error("me: learners query failed", "err", err)
			writeError(w, r, http.StatusInternalServerError, "could not load your account")
			return
		}
		defer rows.Close()
		learners := []learnerDTO{} // [] rather than null in the JSON when there are none
		for rows.Next() {
			var l learnerDTO
			if err := rows.Scan(&l.ID, &l.DisplayName, &l.IsMinor, &l.NativeLanguage, &l.IsAccountHolder); err != nil {
				logFrom(r).Error("me: learners scan failed", "err", err)
				writeError(w, r, http.StatusInternalServerError, "could not load your account")
				return
			}
			learners = append(learners, l)
		}
		if err := rows.Err(); err != nil {
			logFrom(r).Error("me: learners query failed", "err", err)
			writeError(w, r, http.StatusInternalServerError, "could not load your account")
			return
		}

		writeJSON(w, http.StatusOK, map[string]any{
			"account":       a,
			"balance_cents": balance,
			"currency":      "USD",
			"learners":      learners,
		})
	}
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

// validTimezone reports whether name is an IANA time zone Go knows, like
// "America/Bogota". "Local" and "" are refused: they'd mean the server's zone.
func validTimezone(name string) bool {
	if name == "" || name == "Local" || len(name) > 64 {
		return false
	}
	_, err := time.LoadLocation(name)
	return err == nil
}
