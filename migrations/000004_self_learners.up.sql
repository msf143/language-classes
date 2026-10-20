-- Every account holder can attend classes themselves. New accounts get a learner
-- profile at first sign-in (verifyLoginToken in cmd/api/auth.go); this gives one
-- to each existing account that has none. Teacher accounts are skipped.
--
-- IDs are UUIDv7 like everywhere else. The app makes them in Go; here, in SQL,
-- the same layout is built from a random (v4) UUID: overwrite its first 6 bytes
-- with the current Unix time in milliseconds, then set bits 52 and 53 so the
-- version nibble reads 7 (0100 -> 0111).
INSERT INTO learner (id, account_id, display_name, native_language)
SELECT encode(
         set_bit(set_bit(
           overlay(uuid_send(gen_random_uuid())
                   placing substring(int8send((extract(epoch FROM clock_timestamp()) * 1000)::bigint) FROM 3)
                   FROM 1 FOR 6),
           52, 1), 53, 1),
         'hex')::uuid,
       a.id, a.display_name, a.preferred_language
FROM account a
WHERE NOT EXISTS (SELECT 1 FROM learner l WHERE l.account_id = a.id)
  AND NOT EXISTS (SELECT 1 FROM teacher t WHERE t.account_id = a.id);
