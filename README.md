# language-classes

A web app for booking low-cost, live language classes over video. A teacher in
Bogotá, Colombia teaches **Spanish to English speakers** and **English to Spanish
speakers**. Students anywhere in the world can join a scheduled group class or book a
private lesson at a time that suits them.

> **Status:** early development. The class catalog is working. Sign-in, booking and
> payments are next (see [Roadmap](#roadmap)).

## What it does

**Group sessions.** The teacher sets the schedule and students take seats. A group needs
at least 2 students. If fewer than 2 have booked 24 hours before the start, the class is
cancelled automatically and everyone is refunded. Seat caps are optional.

**Private sessions.** A student picks a time from the teacher's open availability and
books for a party of 1–4. One person pays, and the price per person drops as the party
grows. Friends join through an invite link.

**Credits, not per-class card payments.** Students buy dollar-valued credit packs (with a
bonus, e.g. pay $50, get $55) and bookings are paid from that balance. Refunds go back as
credit, and unused credit can be cashed out on request.

**Cancellation policy (summary)**

| Situation                               | Refund                          |
|-----------------------------------------|---------------------------------|
| Student cancels more than 24h before    | 100%                            |
| Student cancels within 24h              | 50%                             |
| Student doesn't show up                 | none                            |
| Teacher cancels, or group is under min. | 100%                            |
| Teacher doesn't show up                 | 100% plus an equal goodwill credit |

**Also:** bilingual interface (English/Spanish), times shown in each visitor's own time
zone, parent accounts with child learner profiles, and CEFR levels (A1–C2) set by the teacher.

## Tech stack

Chosen for "as simple as possible, yet easily adaptable to change": few dependencies,
no frameworks.

| Layer       | Choice                                                                  |
|-------------|-------------------------------------------------------------------------|
| API         | Go, standard library `net/http` and `log/slog`; `pgx` for Postgres      |
| Database    | PostgreSQL 16; plain SQL migrations with `golang-migrate`               |
| Front-end   | Vanilla HTML, CSS and JavaScript, served by the Go server               |
| Dev env     | GitHub Codespaces / VS Code Dev Containers (Go + Postgres containers)   |
| Deployment  | *Planned:* Google Cloud Run, Cloud SQL, Secret Manager, Terraform, GitHub Actions |

## Getting started

The easiest way is a **GitHub Codespace** (or VS Code with the Dev Containers extension):
open the repo and the container setup installs Go, the Postgres client, `jq` and
`migrate`, and starts a Postgres database. `DATABASE_URL` is already set for you.

Then, from the repo root:

```bash
migrate -path migrations -database "$DATABASE_URL" up   # create the schema
psql "$DATABASE_URL" -f scripts/seed.sql                # load sample data (safe to re-run)
go run ./cmd/api                                        # start the server on :8080
```

Open the forwarded port 8080 in your browser to see the class catalog, or try the API:

```bash
curl -s localhost:8080/health | jq
curl -s "localhost:8080/sessions?kind=group&language=es" | jq
```

<details>
<summary>Running outside the dev container</summary>

You need Go (see `go.mod` for the version), PostgreSQL 16 and
[`golang-migrate`](https://github.com/golang-migrate/migrate) built with the `postgres` tag.
Create a database and set:

```bash
export DATABASE_URL="postgres://USER:PASSWORD@localhost:5432/DBNAME?sslmode=disable"
```

Then follow the steps above.
</details>

## Development

```bash
go vet ./... && go build ./... && go test ./...   # check everything before committing
psql "$DATABASE_URL"                              # SQL shell
```

Tests use the standard `testing` package and run against the **real database**, not
mocks, because many business rules are enforced by database constraints. Tests that need
the database are skipped when `DATABASE_URL` isn't set.

**Configuration**

| Variable       | Purpose                              | Default |
|----------------|--------------------------------------|---------|
| `DATABASE_URL` | Postgres connection string           | (set by the dev container) |
| `PORT`         | HTTP port                            | `8080`  |

## API

| Method & path   | Description |
|-----------------|-------------|
| `GET /health`   | `{"status":"ok"}`, or 503 if the database can't be reached |
| `GET /sessions` | Upcoming sessions with seat counts. Optional filters: `kind` (`group`/`private`), `language` (`es`/`en`), `audience` (`adults`/`kids`/`all`). Invalid values return 400. |

Every response has an `X-Request-ID` header (taken from the request if you send one).
Error responses look like `{"error": "...", "request_id": "..."}`. Quote the request ID
when reporting a problem: it matches the server's log lines.

## Project layout

```
.devcontainer/   dev container definition (app + Postgres services)
cmd/api/         the Go server: main.go (wiring, middleware, helpers), ids.go, one file per resource
migrations/      numbered SQL migrations (never edited once applied; add a new number)
scripts/         seed.sql: development sample data
web/             front-end: index.html, styles.css, app.js, i18n.js (all UI text, en + es)
```

## Design principles

A few rules shape the code. [CLAUDE.md](CLAUDE.md) has the full list.

- **Let the database enforce correctness.** Exclusion constraints stop the teacher from
  being double-booked, unique keys make retries safe (idempotency), and CHECK constraints
  keep statuses and amounts valid.
- **Money is integer cents**, never floating point. Prices are snapshotted on each booking.
- **The credit ledger is append-only**, like a journal: corrections are new entries, and
  a balance is the sum of the entries.
- **Derive, don't store.** Seat counts, balances and open time slots are calculated from
  the source rows, not kept in counters that can drift.
- **Nothing is hard-deleted**; records change status instead.
- **Everything is stored in UTC** and converted to local time only for display.
- **Events go through an outbox table** written in the same transaction as the data, so
  emails and reminders are never lost or sent for changes that rolled back.

## Roadmap

1. Sign-in with emailed login links (no passwords)
2. Booking, in one transaction with idempotency keys
3. Teacher availability, with bookable private slots computed from it
4. Background worker for emails, reminders and the 24h group-minimum check
5. Credit packs and payments, credit expiry, and a public policies page
6. First-run setup for the teacher account
7. Deployment to Google Cloud Run

Not planned for now: a roles/permissions system, teacher payouts, reviews, or a message broker.
