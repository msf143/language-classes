# CLAUDE.md

Guidance for Claude Code working in this repository.

## Project

A web app where students worldwide book low-cost, live, interactive language classes
taught over video by a teacher in Bogotá, Colombia. He teaches **Spanish to English
speakers** and **English to Spanish speakers**. He is the only teacher for now, but the
model supports more teachers later.

Two kinds of classes:
- **Group sessions**: the teacher sets the schedule; students take seats. Minimum 2
  students. Max seats is optional per session (null = no cap).
- **Private sessions**: a student books a time from the teacher's open availability.
  Party of 1–4 people; **one person pays**; price per person drops as the party grows.
  Friends join via an invite link and need their own (free) accounts.

## How to work with me (Mike)

- I'm an experienced backend/integration architect (IBM i, DB2, AWS, REST, change-data
  capture) but **new to front-end and new to Go**. I'm here to learn, not just to get code.
- **Explain what you change and why**, especially Go idioms and browser/DOM concepts.
  Relating ideas to databases, journals, or integration patterns helps me.
- I prefer **little abstraction**: standard library over frameworks, plain SQL over ORMs,
  vanilla JS over React. Don't add a dependency without asking.
- My architectural theme: **"as simple as possible, yet easily adaptable to change."**
- Make small, reviewable changes. Run and verify things (build, curl, tests) before
  saying they work. Suggest a commit message after each working step.

## Stack

- **API:** Go, standard library `net/http` (Go 1.22+ routing patterns like `"GET /sessions"`),
  `log/slog` for logging, `github.com/jackc/pgx/v5` (pgxpool) for Postgres. No framework.
- **Database:** Postgres 16. Migrations are plain SQL run by `golang-migrate`.
- **Front-end:** vanilla HTML/CSS/JS in `web/`, served by the Go server for now.
  Bilingual UI (English/Spanish).
- **Dev environment:** GitHub Codespaces via `.devcontainer/` (Go container + Postgres container).
- **Planned deployment:** Google Cloud Run (Go container), Cloud SQL for Postgres, Secret
  Manager, Terraform, GitHub Actions. Front-end later on a CDN.

## Layout

```
.devcontainer/      devcontainer.json + compose.yaml (app + db services)
cmd/api/            main.go    server wiring, middleware, shared helpers (writeJSON, writeError, oneOf)
                    ids.go     newUUIDv7 (primary keys)
                    auth.go    sign-in: login links (newSecret/hashSecret), sessions
                    sessions.go GET /sessions
                    one file per resource as the API grows (bookings.go, ...); *_test.go beside it
migrations/         000NNN_description.up.sql / .down.sql  (numbered, append-only)
scripts/seed.sql    dev seed data (idempotent; times relative to now())
web/                index.html, styles.css, i18n.js (all UI strings), app.js
```

## Commands

```bash
go run ./cmd/api                                      # start API on :8080 (run from repo root; serves ./web)
migrate -path migrations -database "$DATABASE_URL" up # apply migrations
psql "$DATABASE_URL" -f scripts/seed.sql              # load dev seed data
psql "$DATABASE_URL"                                  # SQL shell
curl -s localhost:8080/sessions | jq                  # try the API
go test ./...                                         # run tests (DB tests need DATABASE_URL)
go vet ./... && go build ./... && go test ./...       # check before committing
```

`DATABASE_URL` is set by the devcontainer (`postgres://app:app@db:5432/app?sslmode=disable`).
`APP_ENV=dev` (also set by the devcontainer) makes login links go to the log; without it and
without an email provider, `POST /auth/login` fails on purpose. `APP_BASE_URL` overrides the
site address used in links (default: the Codespace's forwarded 8080 URL, else localhost).

Try sign-in: `curl -s -X POST localhost:8080/auth/login -d '{"email":"ana@example.com"}'`,
then find the link in the log.

## Conventions (follow these)

**Database**
- IDs are **UUIDv7, generated in Go** with `newUUIDv7()` (`cmd/api/ids.go`, standard library only;
  no DB default), so the app knows IDs before insert. `newRequestID()` is for log tracing only,
  never for keys. (The comment in migration 000001 mentions `google/uuid`; that's outdated.)
- All timestamps are `timestamptz`, stored in **UTC**. Convert to local time only for display
  (the front-end does this with `Intl.DateTimeFormat`).
- Money is **integer cents + currency** (USD only for now). Never floats.
- Statuses are `text` + `CHECK` constraints, not Postgres enums.
- **Never hard-delete business records**; change status instead. Tables have `created_at`/`updated_at`
  (an `updated_at` trigger maintains the latter).
- **Snapshot prices on the booking** (what was actually charged), never look them up later.
- **Derive, don't store**: seat counts, account balances, and bookable private slots are
  computed from source rows, not kept in counters.
- Let the database enforce correctness: the exclusion constraint on `session.occupied`
  prevents double-booking a teacher (buffer included); unique idempotency keys prevent
  duplicate bookings/payments.
- `credit_ledger` is **append-only** (a trigger blocks UPDATE/DELETE). Corrections are new entries.
  Balance = `SUM(amount_cents)` (see the `account_balance` view).

**Migrations**
- Never edit a migration that has already been applied. Add the next number
  (`000002_description.up.sql` + `.down.sql`).

**Go / API**
- Every request gets a request ID (`X-Request-ID`, accepted from the client or generated),
  returned in the response header and included in every log line for that request.
- Log errors with the **real cause** (`logFrom(r).Error("what failed", "err", err)`), but send
  clients a generic message via `writeError`, which includes the `request_id`.
- Structured JSON logs to stdout (Cloud Run collects them). The request log includes the query
  string; `redactQuery` masks any parameter whose name contains "token". Name secret
  parameters accordingly (`token`, `invite_token`) so they're never logged.
- Validate query/body input and return 400 with a clear message rather than silently
  returning empty results.
- Always check `rows.Err()` after iterating query results.
- Handlers needing the DB are built as closures: `listSessions(pool) http.HandlerFunc`.
- Respect `PORT` env var; shut down gracefully on SIGTERM.

**Testing**
- Standard library `testing`, table-driven where there are several cases. No test frameworks.
- Test against the **real Postgres** (`DATABASE_URL`), not mocks. Much of the correctness lives
  in constraints and transactions, and only the real database exercises those. Get a pool with
  `testPool(t)` (in `ids_test.go`), which skips the test when `DATABASE_URL` isn't set.
- Tests create their own rows with fresh `newUUIDv7()` IDs and don't depend on seed data, so
  they can run repeatedly against the dev database.
- Handler tests use `net/http/httptest` (call the handler directly; no running server needed).

**Front-end**
- Pattern: one `state` object → `render()` functions derive the DOM from it → one delegated
  click listener. Fetch from the server when data changes; re-render only for UI-only changes.
- Build elements with `createElement`/`textContent` (the `h()` helper). **Never use `innerHTML`
  with data** (XSS).
- All UI text lives in `web/i18n.js` with both `en` and `es` keys; access via `t("key")`.
- Send an `X-Request-ID` header on API calls and show it as a "Reference" in error messages.
- Let forms own their input; read with `new FormData(form)` on submit.

## Business rules

- **Payment model: dollar-valued credits.** Students buy credit packs (with a bonus, e.g.
  pay $50 get $55) and bookings debit the balance. Refunds go back as credit by default;
  cash refund of unused credit on request.
- **Student cancels:** more than 24h before start → full refund; within 24h → 50%; no-show → none.
- **Teacher cancels** (any time) or a group auto-cancels → full refund.
- **Teacher no-show** → full refund **plus** a goodwill credit equal to the full amount paid
  (ledger: one `refund` entry and one `goodwill` entry).
- **Credits expire** after a configurable period (stored as data, not hard-coded). Expiry is a
  new negative ledger entry, never an edit. Which credits expire first and where the setting
  lives get decided in the Payments step (the ledger's `entry_type` CHECK has no `expiry` yet,
  so that needs a migration). The period is shown to students on a public policies page.
- **Group minimum:** checked 24h before start. Fewer than 2 seats booked → auto-cancel with full
  refunds. At 2+ it's confirmed and runs even if someone later drops. Enforced in the schema:
  group sessions must have `min_seats >= 2` (constraint `session_group_min_seats`).
- **Private sessions:** minimum notice 1 hour; party size 1–4; party size locks 24h before start
  (changes before that reprice the booking). If the party shrinks after the lock, the payer
  gets back **50% of each dropped person's share** (same as a within-24h cancel), at the
  locked per-person price. The booking is not repriced.
- **Availability:** teacher publishes blocks (e.g. Tue 9:00–12:00) and the lengths he offers
  (`teacher.offered_durations`). Bookable slots are computed on a 15-minute grid: block minus
  existing sessions minus buffers. Default buffer 5 minutes (`teacher.buffer_minutes`).
- **Pricing** varies by kind × duration × party size only (`price_tier`, versioned by validity window).
- **Children:** a parent's account holds learner profiles; kids never log in. Kids' group
  sessions use `audience = 'kids'`. Policy: a parent nearby for minors on video.
- **Audience values:** `adults` (adults only), `kids` (minors only), and `all` ("All ages" in
  the UI: no age restriction; mixing minors and adults is fine). Catalog filters treat `all` as
  matching both Adults and Kids. At booking, check attendees against `learner.is_minor` for
  `adults`/`kids` sessions; `all` accepts anyone. The parent-nearby policy applies to any minor
  attending, including in `all` sessions.
- **Learner levels** use CEFR (A1–C2), set by the teacher. Session notes are either
  `shared` (student sees) or `private` (teacher only).
- **Accounts required** (no guest checkout). Sign-in via emailed login links, no passwords.
- Public catalog lists **group sessions only**; private sessions belong to whoever booked them.

## Status and roadmap

For what's already built, read the code and `git log`. Next, in order:
1. **Sign-in** (build it ourselves, no Auth0): emailed login links + session cookie. In small steps:
   a. Migration 000003: `login_token` and `web_session` (store only SHA-256 hashes of secrets).
   b. `POST /auth/login` (always answers "check your email"; rate-limited; in dev the link goes
      to the log). The link opens a page with a **Sign in button** that POSTs the token, because
      email scanners open links automatically and would use up a GET-only token.
   c. `POST /auth/verify`: checks the token (unused, unexpired), creates the account on first
      use, sets an `HttpOnly; Secure; SameSite=Lax` cookie.
   d. `GET /me`, `POST /auth/logout`, and middleware that loads the signed-in account.
   e. Sign-in UI (bilingual), then restrict `GET /sessions?kind=private` to the owner.
2. **Deploy early:** multi-stage Dockerfile (distroless), Terraform (small: registry, Cloud SQL,
   secret, service account, Cloud Run, domain), GitHub Actions with Workload Identity Federation,
   region `us-east1`. Email provider (Postmark or Resend, plain HTTPS POST) for real login links.
   Needs from Mike: a domain name and a Google Cloud account with billing.
3. **Booking:** `POST /bookings` in one transaction (check seats, debit credits, insert booking +
   attendees + outbox event), with idempotency keys. First handler + transaction tests go here.
4. **Teacher side:** publish availability blocks; API computes bookable private slots.
5. **Worker:** process `outbox_event` (claim rows with `FOR UPDATE SKIP LOCKED`), send emails and
   reminders, run the 24h group-minimum check, purge expired login tokens and web sessions.
   Same Go binary, `worker` mode, separate Cloud Run service.
6. **Payments:** buy credit packs; processor webhooks (provider TBD; must support payouts to Colombia).
   Credit expiry (see Business rules) and a bilingual **policies page** (cancellation, refunds,
   credit expiry period, children) that reads its numbers from the same config the code uses.
7. **Setup path:** first-run setup that creates the teacher account and profile (bio, buffer,
   offered durations, price tiers). Until then, `scripts/seed.sql` creates the dev teacher.

Deliberately **not** building yet: roles/permissions system, teacher payouts/splits, topics or
levels on sessions, reviews, a message broker (the outbox table is the queue).
