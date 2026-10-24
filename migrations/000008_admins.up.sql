-- Admins (decided 2026-10-24): one flag, not a roles system. Admins see and
-- manage teachers (activate, deactivate, later invite). Set only by the
-- command `api admin` (cmd/api/setup.go), never through the website.
ALTER TABLE account ADD COLUMN is_admin boolean NOT NULL DEFAULT false;

-- 'pending': an invited teacher who has accepted and is filling in a profile,
-- not yet shown to students until an admin activates them (step 7g).
ALTER TABLE teacher DROP CONSTRAINT teacher_status_check;
ALTER TABLE teacher ADD CONSTRAINT teacher_status_check
    CHECK (status IN ('pending', 'active', 'inactive'));
