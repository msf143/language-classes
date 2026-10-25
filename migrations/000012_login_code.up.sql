-- A 6-digit code in every sign-in email, next to the link (decided 2026-10-25):
-- typed into the page that asked for it, so nobody has to switch tabs. The link
-- still works; whichever is used first uses up the login_token (used_at).
--
-- code_hash: SHA-256 of "<login_token.id>:<code>", never the code itself.
-- NULL on rows from before this migration (their links keep working).
-- code_attempts: guesses made, right or wrong; at 5 the code stops working.
ALTER TABLE login_token
    ADD COLUMN code_hash bytea,
    ADD COLUMN code_attempts int NOT NULL DEFAULT 0 CHECK (code_attempts >= 0);
