-- Credit packs students can buy, as data rather than code, so they can change
-- without a release. A pack is never edited or deleted: to change one, retire
-- it (status 'retired') and add a new one. Payments keep what they were sold
-- (see payment.bonus_cents below).
CREATE TABLE credit_pack (
    id              uuid PRIMARY KEY,
    price_cents     int  NOT NULL CHECK (price_cents > 0),   -- what the student pays
    bonus_cents     int  NOT NULL DEFAULT 0 CHECK (bonus_cents >= 0), -- extra credit on top
    currency        text NOT NULL DEFAULT 'USD',
    status          text NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'retired')),
    sort_order      int  NOT NULL DEFAULT 0,                 -- order on the page
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now()
);
CREATE TRIGGER credit_pack_updated BEFORE UPDATE ON credit_pack FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- The packs decided on 2026-10-23: pay $25 → $25 credit, $50 → $55, $100 → $115.
-- In the migration (not the seed) because production needs them too.
INSERT INTO credit_pack (id, price_cents, bonus_cents, sort_order) VALUES
    ('01a20000-0000-7000-8000-000000002500', 2500,    0, 1),
    ('01a20000-0000-7000-8000-000000005000', 5000,  500, 2),
    ('01a20000-0000-7000-8000-000000010000', 10000, 1500, 3);

-- A payment remembers which pack it bought and the bonus it promised at that
-- moment (the snapshot rule, as with booking prices): changing a pack later
-- doesn't change what someone already paid for. Both nullable, so a payment
-- for something other than a pack stays possible.
ALTER TABLE payment
    ADD COLUMN credit_pack_id uuid REFERENCES credit_pack (id),
    ADD COLUMN bonus_cents    int NOT NULL DEFAULT 0 CHECK (bonus_cents >= 0);
