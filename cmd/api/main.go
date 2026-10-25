package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/msf143/language-classes/migrations"
	"github.com/msf143/language-classes/scripts"
)

type ctxKey string

const requestIDKey ctxKey = "request_id"

func main() {
	// Structured JSON logs to stdout: Cloud Run picks these up automatically.
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))

	// ctx is cancelled on Ctrl+C locally or SIGTERM from Cloud Run.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	pool, err := pgxpool.New(ctx, os.Getenv("DATABASE_URL"))
	if err != nil {
		slog.Error("database config invalid", "err", err)
		os.Exit(1)
	}
	defer pool.Close()

	// "" in production; "staging" on dev.hablemos.live; "dev" in the Codespace.
	appEnv := os.Getenv("APP_ENV")

	// `api migrate`: apply new migrations and exit, instead of serving.
	// The Cloud Run migration job runs this before each deploy.
	// `api migrate --seed` then loads the seed data (staging's job only).
	if len(os.Args) > 1 && os.Args[1] == "migrate" {
		err := runMigrations(ctx, pool, migrations.FS)
		if err == nil && len(os.Args) > 2 && os.Args[2] == "--seed" {
			if err = runSeed(ctx, pool, appEnv, scripts.Seed); err == nil {
				slog.Info("seed data loaded")
			}
		}
		if err != nil {
			slog.Error("migrate failed", "err", err)
			pool.Close()
			os.Exit(1) // os.Exit skips deferred calls, hence the explicit Close
		}
		return
	}

	// `api teacher -email ...`: make an account a teacher, then exit (setup.go).
	// On production: a one-off execution of the migrate job with these arguments.
	if len(os.Args) > 1 && os.Args[1] == "teacher" {
		if err := runMakeTeacher(ctx, pool, os.Args[2:]); err != nil {
			slog.Error("teacher command failed", "err", err)
			pool.Close()
			os.Exit(1)
		}
		return
	}

	// `api admin -email ... [-revoke]`: grant or remove admin rights (setup.go).
	if len(os.Args) > 1 && os.Args[1] == "admin" {
		if err := runMakeAdmin(ctx, pool, os.Args[2:]); err != nil {
			slog.Error("admin command failed", "err", err)
			pool.Close()
			os.Exit(1)
		}
		return
	}

	// `api worker`: process due outbox events (emails), then exit. The Cloud Run
	// worker job runs this every minute.
	if len(os.Args) > 1 && os.Args[1] == "worker" {
		if err := runWorker(ctx, pool, appEnv, os.Getenv("RESEND_API_KEY"), baseURL()); err != nil {
			slog.Error("worker failed", "err", err)
			pool.Close()
			os.Exit(1)
		}
		return
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		if err := pool.Ping(ctx); err != nil {
			logFrom(r).Error("health check: database ping failed", "err", err)
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "db_unavailable"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})

	mux.HandleFunc("GET /sessions", signedIn(pool, listSessions(pool)))
	mux.HandleFunc("POST /bookings", requireSignIn(pool, createBooking(pool)))
	mux.HandleFunc("GET /bookings", requireSignIn(pool, listMyBookings(pool)))
	mux.HandleFunc("GET /private-slots", listPrivateSlots(pool))   // public, like the catalog
	mux.HandleFunc("GET /credit-packs", listCreditPacks(pool))     // public: what credit you can buy
	mux.HandleFunc("GET /policies", getPolicies(pool))             // public: the numbers the policies page states
	stripe := newStripe(os.Getenv("STRIPE_SECRET_KEY"), stripeAPI) // nil (payments unavailable) without a key
	webhookSecret := os.Getenv("STRIPE_WEBHOOK_SECRET")
	// Buying credit needs both: the key to open Checkout, the secret to accept
	// Stripe's "paid" (without it, paid credit would never arrive).
	paymentsOn := stripe != nil && webhookSecret != ""
	mux.HandleFunc("POST /payments/checkout", requireSignIn(pool, startCheckout(pool, stripe, baseURL())))
	mux.HandleFunc("GET /payments/{id}", requireSignIn(pool, getPayment(pool)))
	// Stripe calls this (no sign-in: it's checked by Stripe's signature instead).
	mux.HandleFunc("POST /webhooks/stripe", stripeWebhook(pool, webhookSecret))
	mux.HandleFunc("POST /bookings/{id}/cancel", requireSignIn(pool, cancelBooking(pool)))
	mux.HandleFunc("POST /bookings/{id}/seats", requireSignIn(pool, addSeat(pool)))
	mux.HandleFunc("POST /bookings/{id}/seats/{seat}/remove", requireSignIn(pool, removeSeat(pool)))
	mux.HandleFunc("POST /bookings/{id}/seats/{seat}/link", requireSignIn(pool, seatLink(pool, baseURL())))
	mux.HandleFunc("POST /seats/preview", seatPreview(pool)) // public: the link is the proof
	mux.HandleFunc("POST /seats/join", requireSignIn(pool, joinSeat(pool)))
	mux.HandleFunc("POST /seats/{seat}/leave", requireSignIn(pool, leaveSeat(pool)))

	// The teacher's side (teacher.go).
	mux.HandleFunc("POST /teacher/availability", requireTeacher(pool, publishAvailability(pool)))
	mux.HandleFunc("GET /teacher/availability", requireTeacher(pool, listAvailability(pool)))
	mux.HandleFunc("POST /teacher/availability/{id}/withdraw", requireTeacher(pool, withdrawAvailability(pool)))
	mux.HandleFunc("GET /teacher/sessions", requireTeacher(pool, listTeacherSessions(pool)))
	mux.HandleFunc("GET /teacher/profile", requireTeacher(pool, getTeacherProfile(pool)))
	mux.HandleFunc("PUT /teacher/profile", requireTeacher(pool, putTeacherProfile(pool)))
	mux.HandleFunc("GET /teacher/prices", requireTeacher(pool, getTeacherPrices(pool)))
	mux.HandleFunc("POST /teacher/sessions", requireTeacher(pool, createGroupClass(pool)))
	mux.HandleFunc("POST /teacher/sessions/{id}/cancel", requireTeacher(pool, cancelTeacherSession(pool)))
	mux.HandleFunc("PUT /teacher/prices", requireTeacher(pool, putTeacherPrices(pool)))

	// Admins (admin.go): the teachers, and switching them on or off.
	mux.HandleFunc("GET /admin/teachers", requireAdmin(pool, listAdminTeachers(pool)))
	mux.HandleFunc("GET /admin/teachers/{id}", requireAdmin(pool, getAdminTeacher(pool)))
	mux.HandleFunc("POST /admin/teachers/{id}/activate", requireAdmin(pool, setTeacherStatus(pool, true)))
	mux.HandleFunc("POST /admin/teachers/{id}/deactivate", requireAdmin(pool, setTeacherStatus(pool, false)))

	// Which environment this is (the page shows a banner on test sites), and
	// fake credit, which only exists on test sites: on production this route
	// isn't registered at all.
	mux.HandleFunc("GET /env", getEnv(appEnv, paymentsOn))
	if isTestSite(appEnv) {
		mux.HandleFunc("POST /test-credit", requireSignIn(pool, addTestCredit(pool)))
		mux.HandleFunc("POST /test-teacher", requireSignIn(pool, becomeTestTeacher(pool)))
	}

	// How login links reach people. Dev always logs them, so a dev setup can
	// never email a real address by accident. Staging's emails say [TEST].
	sendLoginLink := noLoginSender
	switch {
	case appEnv == "dev":
		sendLoginLink = logLoginLink
	case os.Getenv("RESEND_API_KEY") != "":
		subjectPrefix := ""
		if appEnv == "staging" {
			subjectPrefix = "[TEST] "
		}
		sendLoginLink = resendSender(os.Getenv("RESEND_API_KEY"), resendEndpoint, subjectPrefix)
	}
	mux.HandleFunc("POST /auth/login", requestLoginLink(pool, sendLoginLink, baseURL()))

	// Other emails the API sends itself (teacher invitations, whose secret
	// can't wait in the outbox). Same choice as login links; nil = none.
	var apiMailer mailer
	switch {
	case appEnv == "dev":
		apiMailer = logMailer
	case os.Getenv("RESEND_API_KEY") != "":
		subjectPrefix := ""
		if appEnv == "staging" {
			subjectPrefix = "[TEST] "
		}
		apiMailer = resendMailer(os.Getenv("RESEND_API_KEY"), resendEndpoint, subjectPrefix)
	}
	mux.HandleFunc("POST /admin/invites", requireAdmin(pool, createInvite(pool, apiMailer, baseURL())))
	mux.HandleFunc("GET /admin/invites", requireAdmin(pool, listInvites(pool)))
	mux.HandleFunc("POST /admin/invites/{id}/cancel", requireAdmin(pool, cancelInvite(pool)))
	mux.HandleFunc("POST /invites/accept", acceptInvite(pool)) // public: the link is the proof
	mux.HandleFunc("POST /auth/verify", verifyLoginToken(pool))
	mux.HandleFunc("POST /auth/relink", relinkLogin(pool, sendLoginLink, baseURL())) // an old link asks for a fresh one
	mux.HandleFunc("POST /auth/logout", logout(pool))
	mux.HandleFunc("GET /me", requireSignIn(pool, getMe(pool)))
	mux.HandleFunc("PUT /me", requireSignIn(pool, putMe(pool)))
	mux.HandleFunc("POST /me/welcomed", requireSignIn(pool, markWelcomed(pool)))
	mux.HandleFunc("POST /me/learners", requireSignIn(pool, addLearner(pool)))
	mux.HandleFunc("PUT /me/learners/{id}", requireSignIn(pool, updateLearner(pool)))
	mux.HandleFunc("POST /me/learners/{id}/archive", requireSignIn(pool, archiveLearner(pool)))

	// Serve the front-end (the web folder) for every other GET request.
	mux.Handle("GET /", webFiles("web"))

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	srv := &http.Server{
		Addr:              ":" + port,
		Handler:           withRequestLog(redirectWWW(mux)),
		ReadHeaderTimeout: 5 * time.Second,
	}

	go func() {
		slog.Info("listening", "port", port)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("server failed", "err", err)
			stop()
		}
	}()

	<-ctx.Done()
	slog.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil { // lets in-flight requests finish
		slog.Error("shutdown error", "err", err)
	}
}

// withRequestLog gives every request an ID, returns it in a response header,
// and writes one log line per request with method, path, status and duration.
func withRequestLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get("X-Request-ID")
		if id == "" || len(id) > 64 {
			id = newRequestID()
		}
		w.Header().Set("X-Request-ID", id)

		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		start := time.Now()
		next.ServeHTTP(rec, r.WithContext(context.WithValue(r.Context(), requestIDKey, id)))

		slog.Info("request",
			"request_id", id,
			"method", r.Method,
			"path", r.URL.Path,
			"query", redactQuery(r.URL.Query()),
			"status", rec.status,
			"duration_ms", time.Since(start).Milliseconds(),
		)
	})
}

// redirectWWW sends any request for "www.<site>" to "https://<site>" with the same
// path and query, so the site has one address (better for bookmarks, search
// engines and cookies, which are per host). 308 means "moved permanently, repeat
// the same request there": unlike the older 301, a POST stays a POST.
func redirectWWW(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if bare, ok := strings.CutPrefix(r.Host, "www."); ok {
			http.Redirect(w, r, "https://"+bare+r.URL.RequestURI(), http.StatusPermanentRedirect)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// webFiles serves the page files (HTML, CSS, JS) in dir with
// "Cache-Control: no-cache": browsers may keep a copy but must check with us
// before reusing it. Without that header they guess how long a file stays
// fresh (often hours, from its age), so after a deploy someone could run the
// new index.html with the old app.js. The check is cheap: the browser sends
// If-Modified-Since, and FileServer answers 304 Not Modified (no body) when
// the file hasn't changed. Each image is built from a fresh checkout, so
// every deploy gives the files new modification times.
func webFiles(dir string) http.Handler {
	files := http.FileServer(http.Dir(dir))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-cache")
		files.ServeHTTP(w, r)
	})
}

// redactQuery returns the query string for logging, with the value of any
// parameter whose name contains "token" (login links, invites) replaced,
// so anyone who can read the logs can't reuse a token.
func redactQuery(q url.Values) string {
	for key := range q {
		if strings.Contains(strings.ToLower(key), "token") {
			q.Set(key, "REDACTED")
		}
	}
	return q.Encode()
}

// statusRecorder remembers the status code a handler wrote, for logging.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (s *statusRecorder) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}

// logFrom returns a logger tagged with the current request's ID.
func logFrom(r *http.Request) *slog.Logger {
	id, _ := r.Context().Value(requestIDKey).(string)
	return slog.With("request_id", id)
}

// baseURL is the public address of the site, used to build links in emails.
// APP_BASE_URL wins; in a Codespace it's the forwarded port-8080 address;
// otherwise localhost.
func baseURL() string {
	if u := os.Getenv("APP_BASE_URL"); u != "" {
		return strings.TrimSuffix(u, "/")
	}
	if name := os.Getenv("CODESPACE_NAME"); name != "" {
		return "https://" + name + "-8080." + os.Getenv("GITHUB_CODESPACES_PORT_FORWARDING_DOMAIN")
	}
	return "http://localhost:8080"
}

// newRequestID returns a short random ID for tracing one request through the logs.
// It is not a database key; use newUUIDv7 (ids.go) for those.
func newRequestID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// writeError sends a JSON error body that includes the request ID,
// so a user or the front-end can quote it when reporting a problem.
func writeError(w http.ResponseWriter, r *http.Request, status int, msg string) {
	id, _ := r.Context().Value(requestIDKey).(string)
	writeJSON(w, status, map[string]string{"error": msg, "request_id": id})
}

// writeErrorCode is writeError plus a machine-readable code (e.g. "session_full"),
// so the front-end can show its own translated message instead of msg.
func writeErrorCode(w http.ResponseWriter, r *http.Request, status int, code, msg string) {
	id, _ := r.Context().Value(requestIDKey).(string)
	writeJSON(w, status, map[string]string{"error": msg, "code": code, "request_id": id})
}

func oneOf(v string, allowed ...string) bool {
	for _, a := range allowed {
		if v == a {
			return true
		}
	}
	return false
}
