-- =====================================================================
-- Language Class Platform — initial schema
-- Postgres 15+
--
-- Conventions
--   * IDs are UUIDv7, generated in Go (github.com/google/uuid NewV7).
--     No DB default, so the app always knows the ID before insert
--     (handy for outbox events and idempotent retries).
--   * All timestamps are timestamptz, stored in UTC. Convert at display.
--   * Money is integer cents + currency (USD only for now).
--   * Statuses are text + CHECK, not enums (enums are painful to alter).
--   * Business records are never hard-deleted; status changes instead.
-- =====================================================================

CREATE EXTENSION IF NOT EXISTS btree_gist;   -- lets exclusion constraints mix "=" and "&&"

-- Keeps updated_at honest without relying on every query remembering it.
CREATE FUNCTION set_updated_at() RETURNS trigger AS $$
BEGIN
    NEW.updated_at := now();
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

-- ---------------------------------------------------------------------
-- People
-- ---------------------------------------------------------------------

CREATE TABLE account (
    id                  uuid PRIMARY KEY,
    email               text NOT NULL,
    display_name        text NOT NULL,
    timezone            text NOT NULL DEFAULT 'UTC',          -- IANA name, e.g. 'America/Bogota'
    preferred_language  text NOT NULL DEFAULT 'en'
                        CHECK (preferred_language IN ('en', 'es')),
    status              text NOT NULL DEFAULT 'active'
                        CHECK (status IN ('active', 'disabled')),
    created_at          timestamptz NOT NULL DEFAULT now(),
    updated_at          timestamptz NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX account_email_uq ON account (lower(email));

-- A teacher is an account with a teaching profile and settings.
CREATE TABLE teacher (
    id                  uuid PRIMARY KEY,
    account_id          uuid NOT NULL UNIQUE REFERENCES account (id),
    bio_en              text,
    bio_es              text,
    buffer_minutes      int  NOT NULL DEFAULT 5
                        CHECK (buffer_minutes BETWEEN 0 AND 60),
    offered_durations   int[] NOT NULL DEFAULT '{30,45,60,90}', -- private-session lengths, minutes
    status              text NOT NULL DEFAULT 'active'
                        CHECK (status IN ('active', 'inactive')),
    created_at          timestamptz NOT NULL DEFAULT now(),
    updated_at          timestamptz NOT NULL DEFAULT now()
);

-- A learner is the person actually in class. An account may hold
-- several (a parent with children). Kids never need their own login.
CREATE TABLE learner (
    id                  uuid PRIMARY KEY,
    account_id          uuid NOT NULL REFERENCES account (id),
    display_name        text NOT NULL,
    native_language     text NOT NULL CHECK (native_language IN ('en', 'es')),
    is_minor            boolean NOT NULL DEFAULT false,
    status              text NOT NULL DEFAULT 'active'
                        CHECK (status IN ('active', 'archived')),
    created_at          timestamptz NOT NULL DEFAULT now(),
    updated_at          timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX learner_account_idx ON learner (account_id);

-- Level per learner per target language (CEFR), set by the teacher.
-- Separate table so a learner could someday study both directions.
CREATE TABLE learner_level (
    learner_id          uuid NOT NULL REFERENCES learner (id),
    target_language     text NOT NULL CHECK (target_language IN ('en', 'es')),
    cefr_level          text NOT NULL
                        CHECK (cefr_level IN ('A1','A2','B1','B2','C1','C2')),
    set_by_teacher_id   uuid NOT NULL REFERENCES teacher (id),
    updated_at          timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (learner_id, target_language)
);

-- ---------------------------------------------------------------------
-- Availability & sessions
-- ---------------------------------------------------------------------

-- Teacher publishes blocks; bookable private slots are DERIVED
-- (block minus sessions minus buffers), never stored.
CREATE TABLE availability_block (
    id                  uuid PRIMARY KEY,
    teacher_id          uuid NOT NULL REFERENCES teacher (id),
    period              tstzrange NOT NULL CHECK (NOT isempty(period)),
    status              text NOT NULL DEFAULT 'open'
                        CHECK (status IN ('open', 'withdrawn')),
    created_at          timestamptz NOT NULL DEFAULT now(),
    updated_at          timestamptz NOT NULL DEFAULT now(),
    -- No overlapping open blocks for the same teacher.
    EXCLUDE USING gist (teacher_id WITH =, period WITH &&)
        WHERE (status = 'open')
);

CREATE TABLE session (
    id                  uuid PRIMARY KEY,
    teacher_id          uuid NOT NULL REFERENCES teacher (id),
    kind                text NOT NULL CHECK (kind IN ('group', 'private')),
    target_language     text NOT NULL CHECK (target_language IN ('en', 'es')),
    audience            text NOT NULL DEFAULT 'adults'
                        CHECK (audience IN ('adults', 'kids', 'all')),
    title               text,                                   -- mainly for group sessions
    starts_at           timestamptz NOT NULL,
    ends_at             timestamptz NOT NULL,
    -- Time the teacher is unavailable: the session plus his buffer.
    -- Written by the app (start .. end + buffer). Stored rather than
    -- computed because timestamptz arithmetic isn't immutable, and
    -- the exclusion constraint below needs a plain column.
    occupied            tstzrange NOT NULL,
    min_seats           int NOT NULL DEFAULT 1 CHECK (min_seats >= 1),
    max_seats           int CHECK (max_seats IS NULL OR max_seats >= min_seats), -- NULL = no cap
    status              text NOT NULL DEFAULT 'scheduled'
                        CHECK (status IN ('scheduled', 'confirmed', 'completed', 'cancelled')),
    cancel_reason       text CHECK (cancel_reason IN
                            ('under_minimum', 'teacher_cancelled',
                             'teacher_no_show', 'booking_cancelled')),
    meeting_url         text,
    created_at          timestamptz NOT NULL DEFAULT now(),
    updated_at          timestamptz NOT NULL DEFAULT now(),
    CHECK (ends_at > starts_at),
    CHECK (lower(occupied) = starts_at AND upper(occupied) >= ends_at),
    CHECK ((status = 'cancelled') = (cancel_reason IS NOT NULL)),
    -- The double-booking guard: no two live sessions for one teacher
    -- may overlap, buffer included. The database enforces it, so two
    -- students racing for the same slot can't both win.
    EXCLUDE USING gist (teacher_id WITH =, occupied WITH &&)
        WHERE (status <> 'cancelled')
);
CREATE INDEX session_starts_idx ON session (starts_at) WHERE status IN ('scheduled', 'confirmed');

-- ---------------------------------------------------------------------
-- Pricing & bookings
-- ---------------------------------------------------------------------

-- Price per person by kind × duration × party size.
-- Group sessions use party_size = 1. Tiers are versioned by validity
-- window so price changes never rewrite history.
CREATE TABLE price_tier (
    id                      uuid PRIMARY KEY,
    teacher_id              uuid NOT NULL REFERENCES teacher (id),
    kind                    text NOT NULL CHECK (kind IN ('group', 'private')),
    duration_minutes        int  NOT NULL CHECK (duration_minutes > 0),
    party_size              int  NOT NULL CHECK (party_size BETWEEN 1 AND 4),
    price_per_person_cents  int  NOT NULL CHECK (price_per_person_cents >= 0),
    currency                text NOT NULL DEFAULT 'USD',
    valid                   tstzrange NOT NULL DEFAULT tstzrange(now(), NULL),
    created_at              timestamptz NOT NULL DEFAULT now(),
    EXCLUDE USING gist (teacher_id WITH =, kind WITH =, duration_minutes WITH =,
                        party_size WITH =, valid WITH &&)
);

-- A payer's claim on a session. Price is SNAPSHOTTED here.
CREATE TABLE booking (
    id                      uuid PRIMARY KEY,
    session_id              uuid NOT NULL REFERENCES session (id),
    payer_account_id        uuid NOT NULL REFERENCES account (id),
    party_size              int  NOT NULL CHECK (party_size BETWEEN 1 AND 4),
    price_per_person_cents  int  NOT NULL CHECK (price_per_person_cents >= 0),
    total_cents             int  NOT NULL CHECK (total_cents >= 0),
    currency                text NOT NULL DEFAULT 'USD',
    status                  text NOT NULL DEFAULT 'confirmed'
                            CHECK (status IN ('confirmed', 'cancelled', 'attended', 'no_show')),
    cancelled_at            timestamptz,
    refund_cents            int NOT NULL DEFAULT 0 CHECK (refund_cents >= 0),
    idempotency_key         text NOT NULL,
    created_at              timestamptz NOT NULL DEFAULT now(),
    updated_at              timestamptz NOT NULL DEFAULT now(),
    CHECK (total_cents = price_per_person_cents * party_size),
    CHECK (refund_cents <= total_cents),
    CHECK ((status = 'cancelled') = (cancelled_at IS NOT NULL)),
    UNIQUE (payer_account_id, idempotency_key)
);
CREATE INDEX booking_session_idx ON booking (session_id);

-- The learners in a booking (1–4 for private parties).
-- Payer's own learners join directly; friends join via invite link,
-- so learner_id stays NULL until the invite is accepted.
CREATE TABLE booking_attendee (
    id                  uuid PRIMARY KEY,
    booking_id          uuid NOT NULL REFERENCES booking (id),
    learner_id          uuid REFERENCES learner (id),
    invite_token        text UNIQUE,
    status              text NOT NULL DEFAULT 'joined'
                        CHECK (status IN ('invited', 'joined', 'removed')),
    created_at          timestamptz NOT NULL DEFAULT now(),
    updated_at          timestamptz NOT NULL DEFAULT now(),
    CHECK ((status = 'joined') <= (learner_id IS NOT NULL)),   -- joined implies a learner
    CHECK ((status = 'invited') <= (invite_token IS NOT NULL)) -- invited implies a token
);
CREATE UNIQUE INDEX booking_attendee_learner_uq
    ON booking_attendee (booking_id, learner_id) WHERE learner_id IS NOT NULL;

-- ---------------------------------------------------------------------
-- Money: payments in, balance ledger
-- ---------------------------------------------------------------------

CREATE TABLE payment (
    id                  uuid PRIMARY KEY,
    account_id          uuid NOT NULL REFERENCES account (id),
    provider            text NOT NULL,                 -- 'stripe', 'paypal', ...
    provider_ref        text,                          -- processor's ID, set once known
    amount_cents        int  NOT NULL CHECK (amount_cents > 0),
    currency            text NOT NULL DEFAULT 'USD',
    status              text NOT NULL DEFAULT 'pending'
                        CHECK (status IN ('pending', 'succeeded', 'failed', 'refunded')),
    idempotency_key     text NOT NULL,
    created_at          timestamptz NOT NULL DEFAULT now(),
    updated_at          timestamptz NOT NULL DEFAULT now(),
    UNIQUE (account_id, idempotency_key),
    UNIQUE (provider, provider_ref)                 -- webhook retries can't double-apply
);

-- Append-only journal. Balance = SUM(amount_cents). Never UPDATE or DELETE.
CREATE TABLE credit_ledger (
    id                  uuid PRIMARY KEY,
    account_id          uuid NOT NULL REFERENCES account (id),
    entry_type          text NOT NULL CHECK (entry_type IN
                            ('purchase', 'bonus', 'spend', 'refund',
                             'goodwill', 'cash_out', 'adjustment')),
    amount_cents        int  NOT NULL CHECK (amount_cents <> 0),   -- + credit / - debit
    currency            text NOT NULL DEFAULT 'USD',
    booking_id          uuid REFERENCES booking (id),
    payment_id          uuid REFERENCES payment (id),
    note                text,
    created_at          timestamptz NOT NULL DEFAULT now(),
    CHECK (entry_type NOT IN ('purchase', 'bonus') OR payment_id IS NOT NULL),
    CHECK (entry_type NOT IN ('spend', 'refund')   OR booking_id IS NOT NULL)
);
CREATE INDEX credit_ledger_account_idx ON credit_ledger (account_id, created_at);

-- Enforce append-only at the database level.
CREATE FUNCTION reject_ledger_change() RETURNS trigger AS $$
BEGIN
    RAISE EXCEPTION 'credit_ledger is append-only; post a correcting entry instead';
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER credit_ledger_append_only
    BEFORE UPDATE OR DELETE ON credit_ledger
    FOR EACH ROW EXECUTE FUNCTION reject_ledger_change();

CREATE VIEW account_balance AS
    SELECT account_id, currency, SUM(amount_cents) AS balance_cents
    FROM credit_ledger
    GROUP BY account_id, currency;

-- ---------------------------------------------------------------------
-- Teaching notes
-- ---------------------------------------------------------------------

CREATE TABLE session_note (
    id                  uuid PRIMARY KEY,
    session_id          uuid NOT NULL REFERENCES session (id),
    learner_id          uuid NOT NULL REFERENCES learner (id),
    teacher_id          uuid NOT NULL REFERENCES teacher (id),
    visibility          text NOT NULL CHECK (visibility IN ('shared', 'private')),
    body                text NOT NULL,
    created_at          timestamptz NOT NULL DEFAULT now(),
    updated_at          timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX session_note_learner_idx ON session_note (learner_id, created_at);

-- ---------------------------------------------------------------------
-- Outbox: domain events written in the same transaction as the data.
-- A worker publishes them (emails, reminders, future integrations).
-- ---------------------------------------------------------------------

CREATE TABLE outbox_event (
    id                  uuid PRIMARY KEY,
    aggregate_type      text NOT NULL,          -- 'booking', 'session', ...
    aggregate_id        uuid NOT NULL,
    event_type          text NOT NULL,          -- 'booking_created', 'session_cancelled', ...
    payload             jsonb NOT NULL,
    created_at          timestamptz NOT NULL DEFAULT now(),
    processed_at        timestamptz,
    attempts            int NOT NULL DEFAULT 0
);
CREATE INDEX outbox_unprocessed_idx ON outbox_event (created_at) WHERE processed_at IS NULL;

-- ---------------------------------------------------------------------
-- updated_at triggers
-- ---------------------------------------------------------------------

CREATE TRIGGER account_updated            BEFORE UPDATE ON account            FOR EACH ROW EXECUTE FUNCTION set_updated_at();
CREATE TRIGGER teacher_updated            BEFORE UPDATE ON teacher            FOR EACH ROW EXECUTE FUNCTION set_updated_at();
CREATE TRIGGER learner_updated            BEFORE UPDATE ON learner            FOR EACH ROW EXECUTE FUNCTION set_updated_at();
CREATE TRIGGER learner_level_updated      BEFORE UPDATE ON learner_level      FOR EACH ROW EXECUTE FUNCTION set_updated_at();
CREATE TRIGGER availability_block_updated BEFORE UPDATE ON availability_block FOR EACH ROW EXECUTE FUNCTION set_updated_at();
CREATE TRIGGER session_updated            BEFORE UPDATE ON session            FOR EACH ROW EXECUTE FUNCTION set_updated_at();
CREATE TRIGGER booking_updated            BEFORE UPDATE ON booking            FOR EACH ROW EXECUTE FUNCTION set_updated_at();
CREATE TRIGGER booking_attendee_updated   BEFORE UPDATE ON booking_attendee   FOR EACH ROW EXECUTE FUNCTION set_updated_at();
CREATE TRIGGER payment_updated            BEFORE UPDATE ON payment            FOR EACH ROW EXECUTE FUNCTION set_updated_at();
CREATE TRIGGER session_note_updated       BEFORE UPDATE ON session_note       FOR EACH ROW EXECUTE FUNCTION set_updated_at();