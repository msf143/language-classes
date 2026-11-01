-- Video links, simple version (decided 2026-11-01): a teacher saves one permanent
-- room link (e.g. Google Meet), and each class gets a copy in session.meeting_url,
-- the column students and emails read. Creating a room per class later would change
-- only where that value comes from. Saving a new link updates upcoming classes.
-- Never public: only the people in a class and its teacher see it.
ALTER TABLE teacher ADD COLUMN meeting_link text CHECK (length(meeting_link) <= 300);
