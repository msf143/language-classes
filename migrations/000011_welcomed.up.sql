-- The one-time welcome message (decided 2026-10-25): shown after signing in
-- until the person has seen it; remembered here, so it never comes back on
-- another device. NULL = not seen yet (existing accounts too: they see it once).
ALTER TABLE account ADD COLUMN welcomed_at timestamptz;
