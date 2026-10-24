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
                    email.go   mailer: resendMailer (Idempotency-Key), logMailer (dev); login links
                    notify.go  the worker's emails: booking confirmed / cancelled (bilingual, local times)
                    migrate.go `api migrate` (embedded migrations, one transaction each)
                    sessions.go GET /sessions
                    bookings.go POST /bookings (one transaction, row locks, idempotency key)
                    bookings_private.go private classes in POST /bookings (creates the session)
                    bookings_cancel.go POST /bookings/{id}/cancel (refund rule: refundFor), GET /bookings
                    teacher.go requireTeacher, /teacher/availability (publish, list, withdraw)
                    teacher_profile.go GET/PUT /teacher/profile (name, bio EN/ES, break, offered lengths)
                    teacher_prices.go GET/PUT /teacher/prices (versioned price_tier rows)
                    teacher_classes.go POST /teacher/sessions (new group class), POST /teacher/sessions/{id}/cancel
                    account.go PUT /me (name), /me/learners (add, edit, archive family members)
                    admin.go   requireAdmin; admin routes (teachers: list, profile, activate/deactivate)
                    slots.go   bookableStarts: blocks minus busy sessions → slot start times (pure)
                    worker.go  drainOutbox: claims outbox events (SKIP LOCKED), retries, skips; runWorker
                    payments.go credit packs (GET /credit-packs), POST /payments/checkout
                    policies.go GET /policies: the numbers the policies page states (constants + credit_policy)
                    stripe.go  minimal Stripe REST client (Checkout sessions), net/http only
                    webhooks.go POST /webhooks/stripe: signature check, creditPayment, failPayment
                    checks.go  worker's clock-driven checks: group minimum, reminders, purge
                    setup.go   `api teacher`: make an account a teacher (run as a one-off migrate-job execution)
                    expiry.go  credit expiry: reminder before, then the expiry entry (credit_policy)
                    one file per resource as the API grows (bookings.go, ...); *_test.go beside it
migrations/         000NNN_description.up.sql / .down.sql  (numbered, append-only)
scripts/seed.sql    dev seed data (idempotent; times relative to now()); never on prod
scripts/prod-db.sh  migrate/psql against the prod database through the Cloud SQL Auth Proxy
web/                index.html + app.js (catalog, booking/cancel dialogs, my bookings), signin.html + signin.js
                    (emailed link lands here), teacher.html + teacher.js (teacher dashboard), policies.html + policies.js
                    (public rules, numbers from GET /policies), admin.html + admin.js (teachers: list, profile,
                    activate/deactivate), common.js (shared
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
- **Who collects (decided 2026-10-23):** Mike receives all payments in the US through **Stripe Checkout**
  (cards worldwide, Apple Pay, Google Pay; hosted page, so card data never touches our server) and pays the
  teacher in Colombia himself (e.g. Xoom), outside the app. So the app needs no payouts or splits.
  PayPal/Venmo may come later as a second provider (`payment.provider`).
- **Credit packs (decided 2026-10-23), stored as data:** pay $25 → $25 credit; $50 → $55; $100 → $115.
  Ledger: one `purchase` entry (what was paid) + one `bonus` entry (the extra), both pointing at the payment.
- **Student cancels:** more than 24h before start → full refund; within 24h → 50%; no-show → none.
- **Teacher cancels** (any time) or a group auto-cancels → full refund.
- **Teacher no-show** → full refund **plus** a goodwill credit equal to the full amount paid
  (ledger: one `refund` entry and one `goodwill` entry).
- **Credits expire after inactivity (decided 2026-10-24):** when an account has had **no credit activity
  for 24 months** (no ledger entry other than an expiry: buying, booking, refunds all count), the whole
  remaining balance expires as one `expiry` ledger entry (−balance), never an edit. A reminder email goes
  out **30 days before**. Both numbers are data in the one-row `credit_policy` table (migration 000007),
  read by the worker and the public policies page. Legal note: paid credit may count as a gift
  card/prepaid certificate (federal CARD Act: 5 years; some states ban expiry); Mike to check with an
  accountant/lawyer before launch. Switching to "only bonus credit expires" would change only the amount query.
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
   - Cloudflare API token "Edit zone DNS" (DNS edit, zone read, Email Routing, zone settings; this zone + account) is the Codespaces secret
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
5. **Worker** (done 2026-10-23): process `outbox_event` (claim rows with `FOR UPDATE SKIP LOCKED`), send emails and
   reminders, run the 24h group-minimum check, purge expired login tokens and web sessions.
   Design (agreed 2026-10-23):
   - `api worker` (same binary) as a **Cloud Run Job** started **every minute by Cloud Scheduler** (cron's finest
     grain; ~$0–3/month; emails within ~1 min). Each run drains the outbox, runs the time-based checks, exits.
   - Claim with `FOR UPDATE SKIP LOCKED` (competing consumers). At-least-once + idempotent consumer: Resend
     `Idempotency-Key` = event id. Retries with backoff (1, 2, 4… min, cap 1 h), give up after 10; `last_error`.
   - Emails (bilingual, times in the recipient's account zone): booking_created → payer (+ teacher for private);
     booking_cancelled → payer (+ teacher for private).
   - Time-based: group minimum 24 h before start (under `min_seats` → cancel `under_minimum`, full refunds,
     `booking_cancelled` events; else `confirmed`); one reminder 24 h before (none if booked within 24 h),
     deduped by a unique outbox `dedupe_key`; purge long-expired login tokens and web sessions (not business records).
   - Events older than 2 days on first start: marked processed, not sent (`last_error = 'skipped: stale'`).
   - Gap: no video link yet (`session.meeting_url` unset); reminders say the teacher will send it.
   Steps: [x] 5a save the browser's time zone at sign-in (signin.js sends
   `Intl…timeZone` with the token; `verifyLoginToken` stores it only while the account's zone is still `UTC`,
   checked with `validTimezone` / `time.LoadLocation`); 5b: [x] 5b-1 migration 000005 (outbox `available_at`, `last_error`,
   unique `dedupe_key`; index on due events) + `drainOutbox` (`cmd/api/worker.go`: one event per transaction,
   `FOR UPDATE SKIP LOCKED`, handler → processed / retry with backoff / `skip(reason)`; stale > 48 h skipped;
   gives up after 10). Tests run in a private migrated schema (`migratedSchemaPool`); the two-worker test
   measures handlers running at once (without SKIP LOCKED they only take turns). [x] 5b-2 `mailer` (`email.go`: `resendMailer` with
   `Idempotency-Key`, `logMailer` in dev; login links on top), handlers (`notify.go`: booked / cancelled →
   payer, + teacher for private; bilingual, times in each recipient's zone; keys `<event>:payer|teacher`),
   `api worker` (50 s per run, ≤ 500 events; refuses to start without a mailer). First dev run drained 718:
   651 stale, 52 sent to the log, 15 "no longer confirmed"; [x] 5c time-based checks (`cmd/api/checks.go`, run before the drain in every
   worker run, failures logged not fatal): `checkGroupMinimums` (one tx per class, session locked `FOR UPDATE`,
   never an account lock so it can't deadlock with booking; under minimum → `under_minimum`, full refunds,
   `booking_cancelled` events with `cancelled_by: system`), `queueReminders` (`booking_reminder` with
   `dedupe_key = reminder:<booking>`, `ON CONFLICT DO NOTHING`), `purgeExpired`. Tests in a private schema.
   Seed now also resets seed classes' status on rerun (the worker cancels under-minimum seed classes); [x] 5d `infra/worker.tf` applied 2026-10-23: jobs `worker` / `worker-staging`
   (args `worker`, Resend key, `APP_BASE_URL`), SA `scheduler-invoker` with `run.invoker` on those jobs only,
   Cloud Scheduler `* * * * *` → Run Admin API `jobs:run`. Pipeline updates both worker images after the
   service. Each email logs `email sent` with event, role and provider ID. Step 5 done.
6. **Payments** (done 2026-10-24): buy credit packs; processor webhooks (Stripe Checkout; no payouts needed, see Business rules).
   Steps: [x] 6a migration 000006 (`credit_pack` with the three packs, in the migration so prod gets them;
   `payment.credit_pack_id` + `payment.bonus_cents` snapshot) + `GET /credit-packs` (`cmd/api/payments.go`);
   [x] 6b `POST /payments/checkout` (`cmd/api/payments.go` + `cmd/api/stripe.go`: pending
   payment with the pack snapshot, `ON CONFLICT (account_id, idempotency_key)` for replays → Stripe session via
   form-encoded REST with `Idempotency-Key: checkout-<payment id>`, outside any transaction → `provider_ref` =
   session ID; codes `pack_unavailable`, `already_paid`, `payments_unavailable` (503, no key),
   `payment_provider_error` (502); `STRIPE_SECRET_KEY` env, tests use a fake Stripe); [x] 6c `POST /webhooks/stripe` (`cmd/api/webhooks.go`:
   `verifyStripeSignature` = HMAC-SHA256(whsec, t.raw body), `hmac.Equal`, ±5 min; `STRIPE_WEBHOOK_SECRET` env,
   503 without; completed+paid / async_payment_succeeded → `creditPayment` (payment row `FOR UPDATE`; amount,
   currency and session must match; succeeded + ledger `purchase` + `bonus` from the snapshot + outbox
   `payment_succeeded` → receipt email); expired / async failed → `failPayment` (pending only). Race test with
   a gate: without the lock, 4 deliveries credited $220 for one $50 pack; [x] 6d Buy credit (`GET /payments/{id}`: own payment's status + current balance, 404 otherwise; page: credit +
   Buy credit in the header, pack dialog → checkout → `location.assign`, `pageshow` un-sticks a page restored
   from the back-forward cache, Book dialog offers Buy credit when short; back at `/?paid=<id>` the page polls
   ~1 min (10 × 1 s, then 3 s) for succeeded / failed, `?paid=cancelled` notice; checked in headless Chrome
   with a fake Stripe + signed webhook);
   6e keys + webhook: [x] 6e-1 `infra/stripe.tf` (secrets `stripe-secret-key[-staging]`,
   `stripe-webhook-secret[-staging]`, empty, each env's SA reads only its own) + `payments` flag per env in `envs.tf`
   (wires both into the service in `run.tf`; false until the secrets have values) + `GET /env` `payments`
   (key and webhook secret both set) → page hides Buy credit; procedure in `docs/deploy.md` (Payments);
   [x] 6e-2 Mike (2026-10-24): sandbox key `hablemos-staging-checkout` (Create secret key → Custom permissions,
   Checkout Sessions: Write) + event destination `hablemos-staging` (Snapshot, 4 checkout.session.* events) → values
   in the staging secrets; [x] 6e-3 staging `payments = true`, applied; real sandbox payment on dev.hablemos.live 2026-10-24 ($25 pack:
   checkout → webhook `credited` 41 s later, before the browser returned → page's first poll succeeded → receipt
   email next worker run). Also fixed: `[hidden] { display: none !important; }` (a `display` rule had kept the
   empty paid notice visible). Possible follow-up: `Cache-Control: no-cache` on web files (browsers keep old CSS/JS
   after a deploy);
   [x] 6e-4 production with live keys (done 2026-10-24: Stripe live account active, payments + payouts; restricted live key
   `hablemos-prod-checkout` + event destination `hablemos-prod`; Mike's real $25 purchase went through; prod `payments` back
   to false until step 7). Decided: **test, then off**: prod has no teacher/classes until
   step 7, so payments go on only for one real $25 purchase by Mike, then `payments = false` again until step 7.
   Order: deploy the latest release to prod (Stripe's review may look at the site and the policies page) → Mike
   finishes Stripe verification (live mode) → live key `hablemos-prod-checkout` + event destination `hablemos-prod`
   (https://hablemos.live/webhooks/stripe) into `stripe-secret-key` / `stripe-webhook-secret` → prefix check
   (`_live_`, `whsec_`) → prod `payments = true`, apply → $25 purchase → `payments = false`, apply; 6f credit expiry (rule A, inactivity, 24 months, see Business rules) + policies page: [x] 6f-1 migration 000007
   (`expiry` entry type, debit only; `credit_policy` one-row table 24 / 30; down file round-trips);
   [x] 6f-2 `cmd/api/expiry.go`: `queueExpiryReminders` (outbox `credit_expiry_reminder`, `dedupe_key` =
   account + currency + last-activity time) + `expireCredit` / `expireAccount` (account `FOR UPDATE`, re-read after the
   lock, one `expiry` entry −balance + outbox `credit_expired`); tests incl. a gated race test (without the lock:
   balance −$15); [x] 6f-3 emails (`notifyExpiryReminder` re-reads the standing at send time and skips if the expiry
   date moved or nothing is left; `notifyCreditExpired` uses the amount from the event; dates in the account's zone via
   `dateEN` / `dateES`) + both checks wired into `runWorker` (after the class reminders); run on dev with two demo
   accounts (`expiry-demo-1/2@example.com`); 6f-4 policies: [x] 6f-4a `GET /policies` (`cmd/api/policies.go`, public: refund 24 h / 50 %, group minimum 2 decided
   24 h ahead, private notice 60 min, party ≤ 4, break = longest active teacher's buffer, credit 24 months / 30 days;
   new constants `maxPartySize`, `groupMinStudents`; a test checks them against the schema's CHECKs);
   [x] 6f-4b `web/policies.html` + `policies.js` (bilingual; every number from `GET /policies` and `GET /credit-packs`;
   footer on index with Policies + hola@; policies link in the expiry emails; `reply_to: hola@` on every email in
   `resendMailer`). Deliberately not on the page until built: teacher no-show goodwill, party-size lock/repricing. Contact address (decided 2026-10-24): `hola@hablemos.live`, forwarded by Cloudflare
   Email Routing to Mike's inbox (`infra/inbox.tf`; destination = Codespaces secret `TF_VAR_HOLA_FORWARD_TO`; token
   needs Email Routing Addresses: Edit (account), Email Routing Rules: Edit + Zone Settings: Edit (zone); procedure in
   `docs/deploy.md`): [x] applied + destination verified + test mail received (2026-10-24).
   Credit expiry (see Business rules) and a bilingual **policies page** (cancellation, refunds,
   credit expiry period, children) that reads its numbers from the same config the code uses.
7. **← resume here: Setup path** (then turn prod `payments = true` for good). Before announcing the site: Stripe
   public details (support email hola@; consider a PO box instead of the home address), accountant/lawyer on credit
   expiry and taxes. Design (agreed 2026-10-24): the **teacher manages his own profile, prices and classes on his
   page**; he becomes a teacher only by a command (`api teacher -email … [-name …] [-timezone …]`, `cmd/api/setup.go`)
   run on prod as a one-off execution of the `migrate` job with `--args` (no web path to becoming a teacher).
   Steps: [x] 7a the command (one transaction: find/create account by lower(email), teacher row with lengths
   30/45/60 or reactivate; idempotent; tests; procedure in `docs/deploy.md`); 7b profile: [x] 7b-1 `GET`/`PUT /teacher/profile`
   (`cmd/api/teacher_profile.go`: name 1–60 chars on the account, bio EN/ES ≤ 2000 chars (empty → NULL), break 0–15 min,
   lengths ⊆ `allowedLengths` 30/45/60, sorted + de-duplicated, none = group only; one transaction; unknown fields
   refused; the answer includes `allowed_lengths` + `max_break_minutes` for the form); [x] 7b-2 the form on the
   teacher page ("My profile" first; filled by `fillProfileForm` on load and after a save, never on render; choices
   from the server's limits; checked in headless Chrome incl. a reload); 7c prices: [x] 7c-1 `GET`/`PUT /teacher/prices` (`cmd/api/teacher_prices.go`:
   flat list of cells kind × length × party size; PUT = the whole new list: unchanged → nothing, changed → close the old
   row's `valid` at now() + insert from now(), missing → close; teacher row `FOR UPDATE` serializes saves (gated race
   test: without it the second save fails 500 on the EXCLUDE); $1–$500 per person (no $0: the ledger forbids $0 entries);
   type `priceCellDTO`); [x] 7c-2 "My prices" table on the teacher page (rows group +
   private 1–4, columns 30/45/60; dollars typed, cents via `textToCents` (no floats; "." or ","); group price sent for
   every party size; empty = no price; checked in headless Chrome incl. reload, server logged changed 2 / removed 1) +
   the break sentence everywhere from data: main page via `GET /policies` (`breakNote(min)`, hidden at 0), booking and
   reminder emails via the class teacher's `buffer_minutes` (`breakLineEN/ES`, none at 0); 7d classes: [x] 7d-1 server (`cmd/api/teacher_classes.go`): `POST /teacher/sessions`
   creates a group class (date + from in his zone; title ≤ 100; length 30/45/60; min ≥ 2; max empty or min–30; ≥ 48 h ahead
   `too_soon`; needs a group price for that length `no_price`; overlap → `class_overlap` from the EXCLUDE);
   `POST /teacher/sessions/{id}/cancel` (group or private; his only, else 404; started → 409 `class_started`; again →
   200, nothing more): session `FOR UPDATE` → `cancelWithRefunds` (moved out of `decideGroupSession`, shared: session
   cancelled + full refunds + `booking_cancelled` events with `cancelled_by`), email "The teacher had to cancel", no
   teacher copy of his own cancellation. Tests: booking vs cancel race (safe even without the FOR UPDATE: the UPDATE
   takes the row lock) and worker vs teacher (fails without the FOR UPDATE: the reason was overwritten);
   [x] 7d-2 the page: "New group class" form under My classes (lengths from the server, date min 2 days ahead, errors
   translated: `too_soon`, `no_price`, `class_overlap`) + "Cancel class" on classes not yet started, confirm `<dialog>`
   saying how many seats get a full refund; checked in headless Chrome (incl. a real overlap with a seed class).
   Step 7's teacher side is complete: profile, prices, classes, availability. [ ] 7e rollout: run 7a for the real teacher, he sets everything up, prod payments on.
   Then admins and teacher invitations (design agreed 2026-10-24, built **after** launch, "launch first"):
   - **Admin = one flag**, `account.is_admin` (not a roles system), set only by a command `api admin -email …`
     (one-off job execution, like `api teacher`); Mike and the teacher are the admins. No web path to admin.
   - **Invitations:** an admin invites an email → `teacher_invite` (SHA-256 hash of the secret only, single-use,
     expires after 7 days, bound to that email; sent by the worker) → invitee signs in with that email (normal
     login link) → "Become a teacher" → teacher row with new status **`pending`** (migration: status CHECK gains
     'pending') → fills in profile + prices, not shown to students → an admin **activates**.
   - **Admin page:** teachers with status, a teacher's full profile, Activate / Deactivate, open invitations
     (cancel); each admin action logged with who did it.
   - **Deactivating** hides the teacher from new bookings (slots, unbooked classes) and is **refused while he has
     booked upcoming classes** (the admin page lists them; they're cancelled first, with full refunds).
   Steps: 7f admins: [x] 7f-1 migration 000008 (`account.is_admin`; teacher status gains `pending`; down round-trips)
   + `is_admin` in `accountColumns` (so `/me` and every request carry it) + `requireAdmin` (`cmd/api/admin.go`: 401 / 403
   `not_admin`) + `api admin -email … [-revoke]` (`setup.go`, shares `findOrCreateAccount` with `api teacher`);
   [x] 7f-2 (`cmd/api/admin.go`) `GET /admin/teachers` (pending → active → inactive; upcoming / booked classes, prices),
   `GET /admin/teachers/{id}` (profile, `price_list`, `classes`), `POST …/activate|deactivate` (teacher row `FOR UPDATE`;
   deactivate refused 409 `has_booked_classes` + the classes; idempotent; log line "admin: teacher status changed" with
   the admin). Both booking paths take the teacher row **`FOR SHARE`** (`teacherActiveShared`; lock order account →
   session → teacher), so a booking can't slip past a deactivation (gated race test: without it, $15 charged). The
   catalog shows group classes of active teachers only (hidden, not cancelled; back on reactivation); [x] 7f-3
   `web/admin.html` + `admin.js` (list: pending + active, inactive behind a toggle; one teacher: profile, prices, upcoming
   classes, Activate / Deactivate; a refusal lists the booked classes in the way) + "Admin" link in the main page's
   header for admins; checked in headless Chrome (refused on the seed teacher; activate → deactivate on a retired one).
   Possible later: an `admin_action` table (permanent audit trail; logs keep 30 days);
   [ ] 7g invitations.
   **Order decided 2026-10-24: 8 (My account), then 7g, then 9.** (Found: no screen yet to add family members or friends;
   a new account's name is the part of the email before @, unchangeable.)
   After this, teachers/profiles/prices/classes/availability are data, managed in the app (no deploys); still no
   screens for credit packs, `credit_policy`, the payments switch.
   **Setup path:** first-run setup that creates the teacher account and profile (bio, buffer,
   offered durations, price tiers). Until then, `scripts/seed.sql` creates the dev teacher.

8. **My account** (students): [x] 8a server (`cmd/api/account.go`): `PUT /me` (display name), `POST /me/learners`
   (name, under 18, native language; account `FOR UPDATE` for the ≤ 10 limit → 409 `too_many_learners`),
   `PUT /me/learners/{id}` (mine only, else 404), `POST /me/learners/{id}/archive` (status `archived`, never deleted;
   409 `learner_booked` while in a confirmed class not yet ended; account `FOR UPDATE` first, like booking: gated race
   test fails without it); `/me` learners now include `native_language`; renaming yourself also renames your own
   learner (the one still carrying the account's old name), same transaction; [x] 8b "My Account" dialog (header name is
   a button; name form; people list with Edit / Remove (browser `confirm()`); one add/edit form; every change reloads
   `/me`; checked in headless Chrome: rename, add a child, edit, "Who's attending?", remove). Step 8 done.
9. **Friends in private classes** (business rule: one payer, friends join via invite link with their own free
   accounts): the payer books for a party, gets invite links; a friend signs in and joins with one of their learners;
   price per person follows the party size until the 24 h lock (then 50% back per dropped person). Schema is ready:
   `booking_attendee.status` invited/joined/removed + `invite_token` (store a hash, like login tokens). Shares the
   invite-link idea with 7g.

Deliberately **not** building yet: a roles/permissions system (admins are one flag, see step 7), teacher payouts/splits, topics or
levels on sessions, reviews, a message broker (the outbox table is the queue).
