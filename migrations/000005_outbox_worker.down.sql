DROP INDEX outbox_due_idx;
CREATE INDEX outbox_unprocessed_idx ON outbox_event (created_at) WHERE processed_at IS NULL;
DROP INDEX outbox_dedupe_key_idx;
ALTER TABLE outbox_event
    DROP COLUMN dedupe_key,
    DROP COLUMN last_error,
    DROP COLUMN available_at;
