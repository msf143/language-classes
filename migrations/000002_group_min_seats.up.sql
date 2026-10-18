-- Business rule: a group session needs at least 2 students to run.
-- Private sessions can still have min_seats = 1 (a party of one).
ALTER TABLE session
    ADD CONSTRAINT session_group_min_seats CHECK (kind <> 'group' OR min_seats >= 2);
