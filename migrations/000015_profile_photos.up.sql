-- Profile photos (decided 2026-11-01): one per account, for account holders
-- only; children's learner profiles never have one. Teachers' photos are
-- public; a student's is visible only to their teacher, themselves and admins.
--
-- In Postgres rather than Cloud Storage: no new infrastructure or dependency,
-- covered by the database backups. The server stores only what it re-encoded:
-- a clean JPEG of at most 512 KiB (typically about 50 KB).
--
-- A photo is personal data, not a business record: removing one really deletes
-- it. Replacing one gives it a new id, so its address never changes content
-- and browsers may cache it for good.
CREATE TABLE profile_photo (
    id                  uuid PRIMARY KEY,
    account_id          uuid NOT NULL UNIQUE REFERENCES account (id),
    image               bytea NOT NULL CHECK (octet_length(image) BETWEEN 1 AND 524288),
    created_at          timestamptz NOT NULL DEFAULT now(),
    updated_at          timestamptz NOT NULL DEFAULT now()
);
CREATE TRIGGER profile_photo_updated BEFORE UPDATE ON profile_photo FOR EACH ROW EXECUTE FUNCTION set_updated_at();
