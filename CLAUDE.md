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
- I'll have to explain the infrastructure to others in detail: say what each command does, what
  output to expect, and record procedures and diagrams in `docs/deploy.md`. Mark placeholders
  clearly; prefer commands that paste safely as-is.
- **Pace for my understanding, not for speed**: build in small pieces and stop after each so I can
  read it and ask questions; give a short reading guide (where to start, what each part does).
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
.devcontainer/      devcontainer.json + compose.yaml (app + db services) + post-create.sh (installs gcloud,
                    psql, jq, migrate, Claude Code; Terraform comes from a devcontainer feature)
infra/              bootstrap.sh (one-time: base APIs + Terraform state bucket); Terraform files (*.tf)
docs/               deploy.md: how Terraform provisions and how the deployed app runs (Mermaid diagrams)
cmd/api/            main.go    server wiring, middleware, shared helpers (writeJSON, writeError, oneOf)
                    ids.go     newUUIDv7 (primary keys)
                    auth.go    sign-in: login links, verify, signedIn/requireSignIn middleware, /me, logout
                    email.go   resendSender (login links by email through Resend)
                    migrate.go `api migrate` (embedded migrations, one transaction each)
                    sessions.go GET /sessions
                    bookings.go POST /bookings (one transaction, row locks, idempotency key)
                    bookings_private.go private classes in POST /bookings (creates the session)
                    bookings_cancel.go POST /bookings/{id}/cancel (refund rule: refundFor), GET /bookings
                    teacher.go requireTeacher, /teacher/availability (publish, list, withdraw)
                    slots.go   bookableStarts: blocks minus busy sessions → slot start times (pure)
                    one file per resource as the API grows (bookings.go, ...); *_test.go beside it
migrations/         000NNN_description.up.sql / .down.sql  (numbered, append-only)
scripts/seed.sql    dev seed data (idempotent; times relative to now()); never on prod
scripts/prod-db.sh  migrate/psql against the prod database through the Cloud SQL Auth Proxy
web/                index.html + app.js (catalog, booking/cancel dialogs, my bookings), signin.html + signin.js
                    (emailed link lands here), teacher.html + teacher.js (teacher dashboard), common.js (shared
                    t/h/api helpers), i18n.js (all UI strings), styles.css
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
`APP_ENV=dev` (also set by the devcontainer) makes login links go to the log, even if a key is
set. Otherwise `RESEND_API_KEY` makes them go out by email (`cmd/api/email.go`); with neither,
`POST /auth/login` fails on purpose. `APP_BASE_URL` overrides the
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
- Prod applies `*.up.sql` with `api migrate` (`cmd/api/migrate.go`, files embedded via
  `migrations/embed.go`), each file in **one transaction** with its version bump. So no statements
  that can't run in a transaction (`CREATE INDEX CONCURRENTLY`), and no `BEGIN`/`COMMIT` in files.
- Keep migrations backward-compatible with the previous release (add first, drop later): the
  old revision keeps serving while the new one starts.

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
  they can run repeatedly against the dev database. They tidy up by **status**, never by deleting:
  `cancelWhenDone` (sessions) and `retireWhenDone` (teachers → inactive), so test data never shows up in
  the dev catalog or the private slots.
- Handler tests use `net/http/httptest` (call the handler directly; no running server needed).
- **Race tests must be shown to fail without the lock they test** (remove the `FOR UPDATE`, run, restore).
  When the risky window is short, don't rely on goroutines happening to overlap: hold the row locked from
  the test (a "gate" transaction), wait until all requests are blocked (`pg_stat_activity`), then release
  (see `TestCancelBookingRace`).

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
  (`teacher.offered_durations`: 30, 45, 60). Bookable slots are computed on a 15-minute grid:
  block minus existing sessions. **The teacher's break is inside the class** (the "50-minute hour",
  decided 2026-10-21): a class may end up to `teacher.buffer_minutes` (5) early, so classes sit back
  to back (7–8 PM and 8–9 PM). A session's `occupied` = `[starts_at, ends_at)`, no extra gap.
  Shown to students wherever they book (and on the policies page): "Classes may end up to 5 minutes
  early, so the teacher can take a short break between classes."
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
2. **Deploy early** (done 2026-10-20). Region `us-east1`.
   Facts:
   - Google Cloud project ID `hablemos-live` (number 529979780991), billing linked, $25/month budget
     alert, no organization.
   - Domain `hablemos.live`, registered at Cloudflare. Cloudflare zone ID
     `0a909059c691a0ff1b1f2f1eca481b2e` (DNS records managed by Terraform in `infra/domain.tf`).
   - Cloudflare API token (DNS edit + zone read, this zone only) is the Codespaces secret
     `CLOUDFLARE_API_TOKEN`, so it's an env var in the Codespace. Never print or commit it.
   - Terraform state bucket `gs://hablemos-live-tfstate` (us-east1, versioned).
   - **Resuming after a break or rebuild** (credentials expire or live in the home dir). Mike runs:
     `gcloud auth login --no-launch-browser`, then the application-default login **with the
     site-verification scope** (Terraform's `domain.tf` needs it):
     `gcloud auth application-default login --no-launch-browser --scopes=openid,https://www.googleapis.com/auth/userinfo.email,https://www.googleapis.com/auth/cloud-platform,https://www.googleapis.com/auth/sqlservice.login,https://www.googleapis.com/auth/siteverification`,
     then `gcloud config set project hablemos-live`. Check: `gcloud auth list`, and
     `cd infra && terraform plan` should say "no differences" (Terraform and the proxy use the
     application-default login; `docker push` uses the first). Claude's auto mode may not read
     prod (secret, prod DB), so Mike runs `scripts/prod-db.sh` himself.
   - Built so far (all in `infra/`, applied): Cloud Run service `hablemos-api` (`run.tf`), APIs, Artifact Registry repo `app`
     (`us-east1-docker.pkg.dev/hablemos-live/app`, image `api:257f07a` pushed), service account
     `hablemos-api`, Cloud SQL `hablemos-pg` (Postgres 16, db-f1-micro, connection name
     `hablemos-live:us-east1:hablemos-pg`, db/user `app`), secret `database-url` (full keyword DSN;
     password ephemeral/write-only, never in state), IAM grants (cloudsql.client, secretAccessor).
     How it all fits: `docs/deploy.md`.

   Decisions: app served at the apex `hablemos.live` (`www` redirects to it). Custom domain via Cloud
   Run **domain mapping** (no load balancer), with Cloudflare records **DNS only (grey cloud)**.
   Google domain verification needs a TXT record (add it with the token). Cloud Run reaches Cloud SQL
   through its built-in socket (`host=/cloudsql/PROJECT:REGION:INSTANCE` in `DATABASE_URL`; DB
   password in Secret Manager). Terraform turns on all APIs except the two bootstrap enables. Smallest
   Cloud SQL tier (~$10–12/month, the only real fixed cost). Prod migrations: for now by hand with
   `scripts/prod-db.sh migrate up` (Cloud SQL Auth Proxy on 127.0.0.1:5433, password read from the
   secret); move to a Cloud Run Job with GitHub Actions. Never load `scripts/seed.sql` on prod.

   Steps:
   - [x] Multi-stage distroless Dockerfile.
   - [x] gcloud + Terraform in the devcontainer; `infra/bootstrap.sh` run (APIs + state bucket).
   - [x] Terraform part 1 (done 2026-10-19; live at https://hablemos-api-529979780991.us-east1.run.app,
         also reported as https://hablemos-api-bfox5zpazq-ue.a.run.app), in small applied steps:
     - [x] Foundation: GCS backend, APIs, Artifact Registry, service account (`infra/main.tf`).
     - [x] Build + push image: `TAG=$(git rev-parse --short HEAD)`, `docker build/push`
           `us-east1-docker.pkg.dev/hablemos-live/app/api:$TAG` (commit first).
     - [x] Cloud SQL + `database-url` secret + IAM (`infra/database.tf`).
     - [x] Prod migrations with `scripts/prod-db.sh migrate up` (prod at version 3, 2026-10-19).
     - [x] `infra/run.tf`: Cloud Run service `hablemos-api` (image tag in `local.image_tag`, secret as
           `DATABASE_URL`, min 0 / max 2 instances, public invoker). `/health` → ok.
     - Deploying: push to `main` → staging automatically → approve → production. Rollback and a by-hand
       fallback: `docs/deploy.md`.
   - [x] Terraform part 2 (done 2026-10-20; https://hablemos.live live, certificates issued; `www`
         308-redirects via `redirectWWW` in Go; `APP_BASE_URL=https://hablemos.live`; running `c644394`): Cloudflare provider (DNS records) + Cloud Run domain mapping → https://hablemos.live.
   - [x] GitHub Actions (repo `msf143/language-classes`, ID 1422089795) with Workload Identity Federation:
         push to `main` → test, build, migrate, deploy. No stored Google keys. In small steps:
     - [x] `infra/github.tf` applied (pool `github`, provider `github-actions` limited to
           repo ID + owner ID + `refs/heads/main`, SA `github-deployer` with AR writer, run.developer,
           actAs hablemos-api).
     - [x] Migration job: `api migrate` mode in our binary (no new dependency) + Cloud Run Job `migrate`.
           Deployed `d8c4617`; first execution applied 0, version 3.
     - [x] `.github/workflows/deploy.yml` (test on push/PR with Postgres service; main only:
           build/push `api:<short sha>`, run `migrate` job, update service, curl /health). CI owns the
           image (`ignore_changes` in `run.tf`). First run green 2026-10-20 (`1f668ae`, revision 00004).
   - [x] Email provider: Resend (account: Mike). DNS records in `infra/email.tf` (DKIM, send/rsend
         CNAMEs, DMARC p=none), domain verified; key in secret `resend-api-key` (sending-only,
         hablemos.live); `resendSender` (`cmd/api/email.go`, plain-text bilingual email from
         login@hablemos.live). Real sign-in on https://hablemos.live works (2026-10-20, `ccd5222`).
         Later: tighten DMARC to `quarantine` once mail has flowed cleanly for a while.
3. **Booking** (done 2026-10-21; group sessions): `POST /bookings` in one transaction (check seats, debit credits, insert booking +
   attendees + outbox event), with idempotency keys. First handler + transaction tests go here.
   Design (agreed 2026-10-20): group sessions first; private bookings wait for step 4.
   - Request: `POST /bookings {"session_id", "learner_ids"}` + `Idempotency-Key` header; sign-in required.
   - One transaction, READ COMMITTED + row locks, always in this order: lock payer `account` row
     `FOR UPDATE` (serializes spending) → idempotency lookup (same key → return the existing booking, 200;
     same key, different body → 422) → lock `session` `FOR UPDATE` (serializes seats) → checks (group,
     scheduled/confirmed, not started; learners mine, active, fit audience, not already booked; seats derived)
     → price tier valid now for teacher × kind × duration × **exact party size** (none → `no_price`) →
     balance = ledger sum ≥ total → insert booking, attendees, ledger `spend` (−total), outbox `booking_created`.
   - Errors carry a machine-readable `code` (`session_full`, `session_not_open`, `already_booked`,
     `audience_mismatch`, `insufficient_credit`, `no_price`) so the UI translates them via `i18n.js`.
   - Booking closes at the session's start time.
   - Dev seed: group price tiers ($8 / $12 / $15 per person for 30 / 45 / 60 min, every party size) and a
     $50 `adjustment` credit for Ana.
   Steps:
   - [x] 3a. `POST /bookings` (`cmd/api/bookings.go`) + DB tests incl. three race tests (verified they
         fail with the `FOR UPDATE`s removed) + seed additions (group prices, Ana's $50, seed attendees).
         Seats taken = `SUM(party_size)` of confirmed bookings. Errors via `writeErrorCode`; codes also
         `session_not_found`, `unknown_learner`, `idempotency_key_reused` (422).
   - [x] 3b. "Book" button in the catalog: pick learners, confirm price, show balance.
         Pieces: (1) [x] `GET /me` returns `balance_cents` + `learners`; (2) [x] `GET /sessions` returns
         `prices` per party size (tests now cancel their sessions via `cancelWhenDone`); (2b) [x] every
         account holder has a learner: created at first sign-in, migration 000004 backfills (skips
         teachers); (3) [x] the Book `<dialog>` (pick learners, live price, balance before/after; signed
         out → sign-in first); (4) [x] POST with an Idempotency-Key (new per open / tick change, reused on retry), translated
         error codes, reload balance + seats. Verified in headless Chrome (2026-10-20).
   - [x] 3c. **Staging** at `https://dev.hablemos.live` (done 2026-10-20): second Cloud Run service
         `hablemos-staging`, job `migrate-staging` (runs `migrate --seed`), database/user `staging` on the
         shared instance, secret `database-url-staging`, SA `hablemos-staging`, `APP_ENV=staging` (banner,
         test credit up to $500, `[TEST]` email subjects; staging emails often land in spam at first).
         Terraform: `for_each = local.envs` (`infra/envs.tf`), renames in `infra/moved.tf`. Pipeline: test →
         build once → deploy-staging → **approval** (GitHub environment `production`, reviewer msf143) →
         same image to prod. How to approve and roll back: `docs/deploy.md`.
   - [x] 3d. Cancel a booking: refund rules (>24h full, within 24h 50%) as new ledger entries. Also
         "my bookings": show "You're booked" on catalog cards (today you only learn it by trying)
         and list upcoming bookings with a Cancel button.
         Design (agreed 2026-10-21): `POST /bookings/{id}/cancel`, payer only (others' bookings → 404); no
         idempotency key (status makes it idempotent: cancelling twice returns the booking, no second refund).
         Transaction: lock payer `account` (same first lock as booking) → lock the booking → already cancelled
         → return it; not confirmed / class started → 409 → refund = 100% if start − now() > 24h (exactly 24h
         = 50%), else 50% **rounded up** to the student's favour ((total·50 + 99) / 100) → booking cancelled +
         `cancelled_at` + `refund_cents`, ledger `refund` (+refund), outbox `booking_cancelled`; a private
         session is cancelled too (`cancel_reason = 'booking_cancelled'`). No session lock (a freed seat can't
         oversell). Rule numbers are Go constants (the policies page will read them). Pieces: (1) [x] cancel
         endpoint (`cmd/api/bookings_cancel.go`, `refundFor`) + tests incl. a gated race test; (2) [x] `GET /bookings` (mine as payer, confirmed, upcoming; with session, attendees,
         `refund_if_cancelled_cents` from `refundFor`);
         (3) page: [x] 3a "My bookings" section + "You're booked" tags (`state.myBookings` from GET /bookings,
         loaded by loadMe; ignored when signed out via `myBookings()`); [x] 3b Cancel button + confirmation
         dialog (refund from the server's preview; toast uses the refund in the response), refresh.
         Done 2026-10-21, checked in headless Chrome.
4. **Teacher side** (done 2026-10-21): publish availability blocks; API computes bookable private slots.
   Design (agreed 2026-10-21):
   - Blocks are concrete date ranges (`availability_block.period`), with an optional "repeat weekly for N
     weeks" that creates N ordinary blocks in one transaction. No recurring rules (add later if needed).
     Withdraw = status `withdrawn` (never delete, never edit times); withdrawing doesn't cancel booked classes.
   - The server speaks UTC only; the teacher's page converts from/to his time zone (`America/Bogota`).
   - Slots are derived on request, never stored: a pure Go function (blocks, busy ranges, duration, not-before,
     15-minute grid) → start times; table-tested without a database. Public, like the catalog.
   - Limits: 1-hour minimum notice; only lengths in `offered_durations` that have a private price; window
     up to 8 weeks ahead.
   - Races: the exclusion constraint on `session.occupied` decides; `23P01` → 409 "that time was just taken".
   - Dev seed: availability for the dev teacher (next 2 weeks, Bogotá mornings/evenings) and private prices
     per person by party size (e.g. 60 min: $25 / $20 / $17 / $15 for 1–4 people).
   Steps: [x] 4a teacher API (`cmd/api/teacher.go`: `requireTeacher` → 403 `not_teacher`; POST/GET
   `/teacher/availability` (repeat_weeks ≤ 12 via AddDate in the teacher's zone, one transaction, overlap →
   409 `block_overlap` from the EXCLUDE constraint); POST `/teacher/availability/{id}/withdraw`, idempotent); 4b `GET /private-slots`: [x] 4b-1 pure `bookableStarts` (`cmd/api/slots.go`: half-open spans, merge touching
   blocks, 15-min grid, notBefore; 13 table tests, checked to fail on two injected bugs); [x] 4b-2 `GET /private-slots?duration=60`
   (public; per teacher who offers that length and has a private price; blocks + non-cancelled sessions + DB
   now() + 1h notice → bookableStarts; window default 14 days, max 8 weeks) + seed (private prices $25/$20/$17/$15
   for 60 min etc., weekday blocks 9–12 and 18–21 Bogotá for 14 days with date-based IDs, sessions'
   `occupied = [start, end)`);
   [x] 4c private booking: `POST /bookings` with `{"private": {teacher_id, starts_at, duration_minutes,
   target_language}, learner_ids}` (`cmd/api/bookings_private.go`): offered length, ≥ 1h notice, inside open
   blocks (`range_agg` merges touching ones, same rule as `mergeTouching`) → creates the session (audience from
   the learners' ages) → exclusion constraint settles races (`23P01` → 409 `slot_taken`, nothing charged). Group
   and private share `lockPayerAndFindKey`, `checkLearners`, `priceFor`, `chargeAndWrite` (the only code that
   spends credit). New codes: `slot_unavailable`, `slot_taken`; 4d teacher page: [x] 4d-1 server (publish also accepts {date, from, to} read in the teacher's
   account zone via `time.ParseInLocation`; `GET /teacher/sessions` with attendees + minors; test sites only:
   `POST /test-teacher` links the seed teacher profile to the signed-in account); [x] 4d-2 the page (`web/teacher.html` + `teacher.js`: availability by day
   in the account's zone via Intl `timeZone`, add form → {date, from, to, repeat_weeks}, withdraw chips, classes
   with attendees and the minor note; header "Teacher page" link; banner "Become the test teacher" on test sites); 4e student "book a private class": [x] 4e-1 browsing ("Private classes" section on
   index: language + length 30/45/60, per-teacher price line, slots by day in the visitor's zone, break note;
   possible polish: fewer buttons than every 15 min); [x] 4e-2 a time chip opens the Book dialog with a
   private target (`openBookPrivate`; `bookTarget()` re-reads group classes live), posts `{private: ...}`,
   `slot_taken` / `slot_unavailable` messages, reloads slots, disables Book for a time that's gone; break note in
   the dialog. Step 4 done 2026-10-21, checked in headless Chrome (incl. a two-browser race).
5. **← resume here: Worker:** process `outbox_event` (claim rows with `FOR UPDATE SKIP LOCKED`), send emails and
   reminders, run the 24h group-minimum check, purge expired login tokens and web sessions.
   Same Go binary, `worker` mode, separate Cloud Run service.
6. **Payments:** buy credit packs; processor webhooks (provider TBD; must support payouts to Colombia).
   Credit expiry (see Business rules) and a bilingual **policies page** (cancellation, refunds,
   credit expiry period, children) that reads its numbers from the same config the code uses.
7. **Setup path:** first-run setup that creates the teacher account and profile (bio, buffer,
   offered durations, price tiers). Until then, `scripts/seed.sql` creates the dev teacher.

Deliberately **not** building yet: roles/permissions system, teacher payouts/splits, topics or
levels on sessions, reviews, a message broker (the outbox table is the queue).
