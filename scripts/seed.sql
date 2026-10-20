-- Development seed data. Safe to run repeatedly: fixed IDs + ON CONFLICT.
-- Session times are relative to now() and refreshed on every run (ON CONFLICT DO UPDATE),
-- so re-running the seed brings classes back into the future.
-- Usage: psql "$DATABASE_URL" -f scripts/seed.sql

BEGIN;

-- Accounts: one teacher, two students
INSERT INTO account (id, email, display_name, timezone, preferred_language) VALUES
  ('01900000-0000-7000-8000-000000000001', 'teacher@example.com', 'Profesor',      'America/Bogota',   'es'),
  ('01900000-0000-7000-8000-000000000002', 'ana@example.com',     'Ana (student)', 'America/New_York', 'en'),
  ('01900000-0000-7000-8000-000000000003', 'luis@example.com',    'Luis (student)','America/Bogota',   'es')
ON CONFLICT (id) DO NOTHING;

INSERT INTO teacher (id, account_id, bio_en, bio_es) VALUES
  ('01900000-0000-7000-8000-0000000000a1', '01900000-0000-7000-8000-000000000001',
   'Native Spanish speaker from Bogotá.', 'Hablante nativo de español de Bogotá.')
ON CONFLICT (id) DO NOTHING;

INSERT INTO learner (id, account_id, display_name, native_language) VALUES
  ('01900000-0000-7000-8000-0000000000c1', '01900000-0000-7000-8000-000000000002', 'Ana',  'en'),
  ('01900000-0000-7000-8000-0000000000c2', '01900000-0000-7000-8000-000000000003', 'Luis', 'es')
ON CONFLICT (id) DO NOTHING;

-- Ana's daughter: a child learner, so audience rules (adults / kids / all) show up in dev.
INSERT INTO learner (id, account_id, display_name, native_language, is_minor) VALUES
  ('01900000-0000-7000-8000-0000000000c3', '01900000-0000-7000-8000-000000000002', 'Sofía', 'en', true)
ON CONFLICT (id) DO NOTHING;

-- Sessions. occupied = start .. end + 5-minute buffer.
-- Keep every session's hour distinct: on a re-run, rows move one at a time and the
-- no-double-booking constraint checks each move, so a shared hour on different days
-- could collide with another row's old slot.
INSERT INTO session (id, teacher_id, kind, target_language, audience, title,
                     starts_at, ends_at, occupied, min_seats, max_seats, status, cancel_reason)
SELECT v.id::uuid, '01900000-0000-7000-8000-0000000000a1', v.kind, v.lang, v.audience, v.title,
       t.starts, t.starts + v.mins * interval '1 minute',
       tstzrange(t.starts, t.starts + (v.mins + 5) * interval '1 minute'),
       v.min_seats, v.max_seats, v.status, v.reason
FROM (VALUES
  -- id                                      kind       lang  audience  title                                   day hour mins min max   status       reason
  ('01900000-0000-7000-8000-0000000000b1', 'group',   'es', 'adults', 'Conversational Spanish – Beginners',     1, 23, 60, 2, 8,    'scheduled', NULL),
  ('01900000-0000-7000-8000-0000000000b2', 'group',   'en', 'adults', 'English Conversation – Intermediate',   2, 13, 60, 2, NULL, 'scheduled', NULL),
  ('01900000-0000-7000-8000-0000000000b3', 'private', 'es', 'adults', NULL,                                     3, 15, 45, 1, 4,    'scheduled', NULL),
  ('01900000-0000-7000-8000-0000000000b4', 'group',   'es', 'kids',   'Spanish for Kids',                       4, 21, 30, 2, 6,    'scheduled', NULL),
  ('01900000-0000-7000-8000-0000000000b7', 'group',   'es', 'all',    'Family Spanish – All Ages',              6, 16, 45, 2, 10,   'scheduled', NULL),
  ('01900000-0000-7000-8000-0000000000b5', 'group',   'es', 'adults', 'Past class (should not appear)',        -1, 19, 60, 2, 8,    'completed', NULL),
  ('01900000-0000-7000-8000-0000000000b6', 'group',   'en', 'adults', 'Cancelled class (should not appear)',    5, 18, 60, 2, 8,    'cancelled', 'under_minimum')
) AS v(id, kind, lang, audience, title, day, hr, mins, min_seats, max_seats, status, reason)
CROSS JOIN LATERAL (
  SELECT date_trunc('day', now()) + v.day * interval '1 day' + v.hr * interval '1 hour' AS starts
) AS t
ON CONFLICT (id) DO UPDATE SET   -- EXCLUDED = the row this INSERT tried to add
  starts_at = EXCLUDED.starts_at,
  ends_at   = EXCLUDED.ends_at,
  occupied  = EXCLUDED.occupied;

-- Bookings: Ana takes a seat in the beginners group; Luis books a private for 2.
INSERT INTO booking (id, session_id, payer_account_id, party_size,
                     price_per_person_cents, total_cents, idempotency_key) VALUES
  ('01900000-0000-7000-8000-0000000000d1', '01900000-0000-7000-8000-0000000000b1',
   '01900000-0000-7000-8000-000000000002', 1, 800, 800, 'seed-1'),
  ('01900000-0000-7000-8000-0000000000d2', '01900000-0000-7000-8000-0000000000b3',
   '01900000-0000-7000-8000-000000000003', 2, 1200, 2400, 'seed-2')
ON CONFLICT (id) DO NOTHING;

-- Who attends those bookings. Luis's private party of 2: Luis, plus a friend invited but not joined yet.
INSERT INTO booking_attendee (id, booking_id, learner_id, invite_token, status) VALUES
  ('01900000-0000-7000-8000-0000000000e1', '01900000-0000-7000-8000-0000000000d1',
   '01900000-0000-7000-8000-0000000000c1', NULL, 'joined'),
  ('01900000-0000-7000-8000-0000000000e2', '01900000-0000-7000-8000-0000000000d2',
   '01900000-0000-7000-8000-0000000000c2', NULL, 'joined'),
  ('01900000-0000-7000-8000-0000000000e3', '01900000-0000-7000-8000-0000000000d2',
   NULL, 'seed-invite-token-1', 'invited')
ON CONFLICT (id) DO NOTHING;

-- Group prices per person: $8 / $12 / $15 for 30 / 45 / 60 minutes, the same for every
-- party size (no family discount). IDs 01900000-...-000000000NMS: N=1, M=minutes, S=size.
INSERT INTO price_tier (id, teacher_id, kind, duration_minutes, party_size, price_per_person_cents)
SELECT format('01900000-0000-7000-8000-%s', lpad((100000 + d.minutes * 10 + size)::text, 12, '0'))::uuid,
       '01900000-0000-7000-8000-0000000000a1', 'group', d.minutes, size, d.cents
FROM (VALUES (30, 800), (45, 1200), (60, 1500)) AS d(minutes, cents)
CROSS JOIN generate_series(1, 4) AS size
ON CONFLICT (id) DO NOTHING;

-- Ana starts with $50 of credit. An 'adjustment', since 'purchase' requires a real payment row.
INSERT INTO credit_ledger (id, account_id, entry_type, amount_cents, note) VALUES
  ('01900000-0000-7000-8000-0000000000f1', '01900000-0000-7000-8000-000000000002',
   'adjustment', 5000, 'dev seed: starting credit')
ON CONFLICT (id) DO NOTHING;

COMMIT;