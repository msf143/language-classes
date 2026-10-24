-- Credit expiry (decided 2026-10-24): when an account has had no credit
-- activity (no ledger entry other than an expiry) for credit_policy's
-- expire_after_months, the worker writes one 'expiry' entry for the remaining
-- balance. Like every correction, it's a new journal entry, never an edit.

-- 'expiry' joins the entry types. A CHECK can't be altered in place, so it's
-- dropped and re-added (Postgres re-checks every existing row while adding it).
ALTER TABLE credit_ledger DROP CONSTRAINT credit_ledger_entry_type_check;
ALTER TABLE credit_ledger ADD CONSTRAINT credit_ledger_entry_type_check CHECK (entry_type IN
    ('purchase', 'bonus', 'spend', 'refund', 'goodwill', 'cash_out', 'adjustment', 'expiry'));
-- Expiry only ever takes credit away.
ALTER TABLE credit_ledger ADD CONSTRAINT credit_ledger_expiry_is_debit
    CHECK (entry_type <> 'expiry' OR amount_cents < 0);

-- The expiry rule's numbers, as data: the worker and the policies page read
-- this row, so they can't disagree, and changing them needs no release. Exactly
-- one row: the key can only be true, so a second row would repeat it.
CREATE TABLE credit_policy (
    id                  boolean PRIMARY KEY DEFAULT true CHECK (id),
    expire_after_months int NOT NULL CHECK (expire_after_months > 0),   -- inactivity before credit expires
    warn_days_before    int NOT NULL CHECK (warn_days_before >= 0),     -- reminder email this many days earlier (0 = none)
    created_at          timestamptz NOT NULL DEFAULT now(),
    updated_at          timestamptz NOT NULL DEFAULT now()
);
CREATE TRIGGER credit_policy_updated BEFORE UPDATE ON credit_policy FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- Decided 2026-10-24: 24 months without activity; a reminder 30 days before.
INSERT INTO credit_policy (expire_after_months, warn_days_before) VALUES (24, 30);
