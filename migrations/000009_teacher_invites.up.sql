-- Teacher invitations (step 7g): an admin invites an email address; the link
-- in the email (or shown once to the admin) lets that person sign in and become
-- a 'pending' teacher, until an admin activates them.
--
-- Like login tokens, only a SHA-256 hash of the secret is stored: a copy of the
-- database contains nothing anyone could accept an invitation with. Unlike
-- them, invitations are business records (who invited whom, when): never
-- deleted, only moved from 'open' to 'accepted' or 'cancelled'.
CREATE TABLE teacher_invite (
    id                  uuid PRIMARY KEY,
    email               text NOT NULL,
    token_hash          bytea NOT NULL UNIQUE,
    invited_by          uuid NOT NULL REFERENCES account (id),
    status              text NOT NULL DEFAULT 'open'
                        CHECK (status IN ('open', 'accepted', 'cancelled')),
    expires_at          timestamptz NOT NULL,
    accepted_at         timestamptz,
    accepted_account_id uuid REFERENCES account (id),
    created_at          timestamptz NOT NULL DEFAULT now(),
    updated_at          timestamptz NOT NULL DEFAULT now(),
    CHECK (expires_at > created_at),
    CHECK ((status = 'accepted') = (accepted_at IS NOT NULL AND accepted_account_id IS NOT NULL))
);
-- At most one open invitation per address (inviting again replaces it).
CREATE UNIQUE INDEX teacher_invite_open_email_uq ON teacher_invite (lower(email)) WHERE status = 'open';
CREATE TRIGGER teacher_invite_updated BEFORE UPDATE ON teacher_invite FOR EACH ROW EXECUTE FUNCTION set_updated_at();
