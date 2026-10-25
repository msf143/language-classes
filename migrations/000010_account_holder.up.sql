-- The learner who is the account holder themselves (decided 2026-10-25): marked,
-- not guessed from names, so "(you)" and "renamed with you" are always right,
-- and the account holder can't be removed from their own account (they simply
-- don't tick themselves when booking only for others).
ALTER TABLE learner ADD COLUMN is_account_holder boolean NOT NULL DEFAULT false;

-- Existing accounts: the adult learner with the account's own name (everyone's
-- self-entry starts with it, and renaming yourself renames it too); failing
-- that, the earliest adult learner. DISTINCT ON keeps the first row per
-- account in the ORDER BY.
UPDATE learner SET is_account_holder = true
WHERE id IN (
    SELECT DISTINCT ON (l.account_id) l.id
    FROM learner l JOIN account a ON a.id = l.account_id
    WHERE NOT l.is_minor
    ORDER BY l.account_id, (l.display_name = a.display_name) DESC, l.created_at
);

-- You stay on your account: anyone who had removed themselves is back.
UPDATE learner SET status = 'active' WHERE is_account_holder AND status = 'archived';

-- At most one account holder per account.
CREATE UNIQUE INDEX learner_account_holder_uq ON learner (account_id) WHERE is_account_holder;
