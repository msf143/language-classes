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
cmd/api/            main.go (server, middleware, helpers), sessions.go (GET /sessions)
migrations/         000001_init.up.sql / .down.sql  (numbered, append-only)
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
go vet ./... && go build ./...                        # check before committing
```

`DATABASE_URL` is set by the devcontainer (`postgres://app:app@db:5432/app?sslmode=disable`).

## Conventions (follow these)

**Database**
- IDs are **UUIDv7, generated in Go** (no DB default), so the app knows IDs before insert.
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
- Structured JSON logs to stdout (Cloud Run collects them).
- Validate query/body input and return 400 with a clear message rather than silently
  returning empty results.
- Always check `rows.Err()` after iterating query results.
- Handlers needing the DB are built as closures: `listSessions(pool) http.HandlerFunc`.
- Respect `PORT` env var; shut down gracefully on SIGTERM.

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
- **Teacher no-show** → full refund plus a small goodwill credit.
- **Group minimum:** checked 24h before start. Fewer than 2 seats booked → auto-cancel with full
  refunds. At 2+ it's confirmed and runs even if someone later drops.
- **Private sessions:** minimum notice 1 hour; party size 1–4; party size locks 24h before start
  (changes before that reprice the booking).
- **Availability:** teacher publishes blocks (e.g. Tue 9:00–12:00) and the lengths he offers
  (`teacher.offered_durations`). Bookable slots are computed on a 15-minute grid: block minus
  existing sessions minus buffers. Default buffer 5 minutes (`teacher.buffer_minutes`).
- **Pricing** varies by kind × duration × party size only (`price_tier`, versioned by validity window).
- **Children:** a parent's account holds learner profiles; kids never log in. Kids' group
  sessions use `audience = 'kids'`. Policy: a parent nearby for minors on video.
- **Learner levels** use CEFR (A1–C2), set by the teacher. Session notes are either
  `shared` (student sees) or `private` (teacher only).
- **Accounts required** (no guest checkout). Sign-in via emailed login links, no passwords.
- Public catalog lists **group sessions only**; private sessions belong to whoever booked them.

## Status and roadmap

Done:
1. Dev environment (Codespaces, Postgres), schema migration 000001, seed data
2. API skeleton: `/health`, request-ID logging, graceful shutdown
3. `GET /sessions` (filters: kind, language, audience; seat counts)
4. Class catalog page (vanilla JS, bilingual, local time zones, filters, error state with reference ID)

Next, in order:
1. **Sign-in:** email login link + session cookie. In dev, print the link to the log (no email
   provider yet). Then restrict `GET /sessions?kind=private` to the owner.
2. **Booking:** `POST /bookings` in one transaction (check seats, debit credits, insert booking +
   attendees + outbox event), with idempotency keys. First real tests go here.
3. **Teacher side:** publish availability blocks; API computes bookable private slots.
4. **Worker:** process `outbox_event` (claim rows with `FOR UPDATE SKIP LOCKED`), send emails and
   reminders, run the 24h group-minimum check. Same Go binary, `worker` mode, separate Cloud Run service.
5. **Payments:** buy credit packs; processor webhooks (provider TBD; must support payouts to Colombia).
6. **Deployment:** multi-stage Dockerfile (distroless), Terraform, Cloud Run, Cloud SQL, GitHub Actions.

Deliberately **not** building yet: roles/permissions system, teacher payouts/splits, topics or
levels on sessions, reviews, a message broker (the outbox table is the queue).
