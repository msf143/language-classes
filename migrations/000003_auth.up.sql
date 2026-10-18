-- Sign-in with emailed login links (no passwords).
--
-- Both tables store a SHA-256 hash of each secret, never the secret itself.
-- The raw value only travels in the email link or the browser cookie, so a
-- copy of the database (backup, leak) contains nothing anyone can log in with.
--
-- These are security records, not business records: expired rows may be
-- purged later by the worker.

-- One row per login link sent. email, not account_id, because the first
-- link is also how a new person signs up (the account doesn't exist yet).
CREATE TABLE login_token (
    id                  uuid PRIMARY KEY,
    email               text NOT NULL,
    token_hash          bytea NOT NULL UNIQUE,
    expires_at          timestamptz NOT NULL,
    used_at             timestamptz,             -- set once; a used link can't be reused
    created_at          timestamptz NOT NULL DEFAULT now(),
    CHECK (expires_at > created_at)
);
-- For "how many links were sent to this address recently" (rate limiting).
CREATE INDEX login_token_email_idx ON login_token (lower(email), created_at);

-- A signed-in browser. Named web_session so it isn't confused with a class
-- session. Revoking (logout) sets revoked_at; the row stays as an audit trail.
CREATE TABLE web_session (
    id                  uuid PRIMARY KEY,
    account_id          uuid NOT NULL REFERENCES account (id),
    token_hash          bytea NOT NULL UNIQUE,
    expires_at          timestamptz NOT NULL,
    revoked_at          timestamptz,
    created_at          timestamptz NOT NULL DEFAULT now(),
    CHECK (expires_at > created_at)
);
CREATE INDEX web_session_account_idx ON web_session (account_id);
