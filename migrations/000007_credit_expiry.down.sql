-- Fails while any 'expiry' entries exist: the ledger is append-only, so they
-- can't be removed, and the old CHECK wouldn't allow them.
DROP TABLE credit_policy;
ALTER TABLE credit_ledger DROP CONSTRAINT credit_ledger_expiry_is_debit;
ALTER TABLE credit_ledger DROP CONSTRAINT credit_ledger_entry_type_check;
ALTER TABLE credit_ledger ADD CONSTRAINT credit_ledger_entry_type_check CHECK (entry_type IN
    ('purchase', 'bonus', 'spend', 'refund', 'goodwill', 'cash_out', 'adjustment'));
