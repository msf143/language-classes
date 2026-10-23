-- What the worker (cmd/api/worker.go) needs to treat outbox_event as a work queue.
--
--   available_at  when the event may (next) be tried. New events: right away.
--                 A failed attempt moves it forward (backoff: 1, 2, 4 ... minutes).
--   last_error    why the last attempt failed, or why the event was skipped.
--   dedupe_key    optional; unique when set. Lets a producer say "queue this only
--                 once" (e.g. one reminder per booking): a second insert with the
--                 same key fails instead of creating a duplicate.
--
-- All three are additive with defaults, so code that doesn't know them keeps
-- working (the previous release keeps serving while the new one starts).
ALTER TABLE outbox_event
    ADD COLUMN available_at timestamptz NOT NULL DEFAULT now(),
    ADD COLUMN last_error   text,
    ADD COLUMN dedupe_key   text;

CREATE UNIQUE INDEX outbox_dedupe_key_idx ON outbox_event (dedupe_key) WHERE dedupe_key IS NOT NULL;

-- The worker asks "which unprocessed events are due?", so index exactly that.
DROP INDEX outbox_unprocessed_idx;
CREATE INDEX outbox_due_idx ON outbox_event (available_at) WHERE processed_at IS NULL;
