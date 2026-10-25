-- Passkeys (decided 2026-10-25): sign in with Face ID / fingerprint / the
-- device's PIN instead of an emailed code or link. The emailed sign-in stays:
-- it's how an account starts, and the way back in on a new device.
--
-- How it works (WebAuthn): when someone adds a passkey, their device makes a
-- key pair. The private key never leaves the device (or their iCloud / Google
-- password manager, which syncs it). We keep only the public key. To sign in,
-- we send a random challenge; the device signs it after Face ID; we check the
-- signature with the public key. So there's no secret on our side at all: a
-- copy of this table lets nobody sign in.

-- One row per passkey. Removing one sets status 'removed' (never deleted);
-- a removed passkey can't sign in.
CREATE TABLE passkey (
    id                  uuid PRIMARY KEY,
    account_id          uuid NOT NULL REFERENCES account (id),
    credential_id       bytea NOT NULL UNIQUE,  -- the device's name for this key; sent back at each sign-in
    public_key          bytea NOT NULL,         -- SubjectPublicKeyInfo (DER), as the browser hands it over
    algorithm           int NOT NULL CHECK (algorithm IN (-7, -257)),  -- COSE: -7 ES256 (P-256), -257 RS256
    sign_count          bigint NOT NULL DEFAULT 0 CHECK (sign_count >= 0),  -- the device's counter (synced passkeys stay at 0)
    name                text NOT NULL CHECK (length(name) BETWEEN 1 AND 60),  -- shown in My Account, e.g. "iPhone · Safari"
    status              text NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'removed')),
    last_used_at        timestamptz,
    created_at          timestamptz NOT NULL DEFAULT now(),
    updated_at          timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX passkey_account_idx ON passkey (account_id);
CREATE TRIGGER passkey_updated BEFORE UPDATE ON passkey FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- One-time challenges: the random bytes a device signs. Each works once
-- (used_at), for a few minutes, for one purpose: 'register' (adding a passkey
-- to the signed-in account_id) or 'signin' (no account yet: whoever's passkey
-- signs it). Not secret, but single-use, so a signature can't be replayed.
-- Security records, like login_token: the worker may purge old rows.
CREATE TABLE passkey_challenge (
    id                  uuid PRIMARY KEY,
    challenge           bytea NOT NULL UNIQUE CHECK (length(challenge) = 32),
    purpose             text NOT NULL CHECK (purpose IN ('register', 'signin')),
    account_id          uuid REFERENCES account (id),
    expires_at          timestamptz NOT NULL,
    used_at             timestamptz,
    created_at          timestamptz NOT NULL DEFAULT now(),
    CHECK (expires_at > created_at),
    CHECK ((purpose = 'register') = (account_id IS NOT NULL))  -- registering needs an account; signing in has none yet
);
CREATE INDEX passkey_challenge_expires_idx ON passkey_challenge (expires_at);
