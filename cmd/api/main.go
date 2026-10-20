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

	// `api migrate`: apply new migrations and exit, instead of serving.
	// The Cloud Run migration job runs this before each deploy.
	if len(os.Args) > 1 && os.Args[1] == "migrate" {
		if err := runMigrations(ctx, pool, migrations.FS); err != nil {
			slog.Error("migrate failed", "err", err)
			pool.Close()
			os.Exit(1) // os.Exit skips deferred calls, hence the explicit Close
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

	sendLoginLink := noLoginSender
	if os.Getenv("APP_ENV") == "dev" {
		sendLoginLink = logLoginLink
	}
	mux.HandleFunc("POST /auth/login", requestLoginLink(pool, sendLoginLink, baseURL()))
	mux.HandleFunc("POST /auth/verify", verifyLoginToken(pool))
	mux.HandleFunc("POST /auth/logout", logout(pool))
	mux.HandleFunc("GET /me", requireSignIn(pool, getMe))

	// Serve the front-end (the web folder) for every other GET request.
	mux.Handle("GET /", http.FileServer(http.Dir("web")))

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

func oneOf(v string, allowed ...string) bool {
	for _, a := range allowed {
		if v == a {
			return true
		}
	}
	return false
}
