-- Fails while any teacher is 'pending' (the old CHECK doesn't allow it).
ALTER TABLE teacher DROP CONSTRAINT teacher_status_check;
ALTER TABLE teacher ADD CONSTRAINT teacher_status_check CHECK (status IN ('active', 'inactive'));
ALTER TABLE account DROP COLUMN is_admin;
