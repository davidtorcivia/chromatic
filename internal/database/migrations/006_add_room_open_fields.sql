-- Scheduled-room lobby support.
--
-- early_open_minutes: how many minutes before scheduled_at guests may enter
-- the countdown lobby (validated 0-120 by the handlers; default 10, matching
-- the previously hardcoded early-access window).
--
-- opened_at: set when an admin opens the room ahead of schedule (POST
-- /api/rooms/{slug}/open) or when the first stream arrives. NULL = the room
-- opens automatically at scheduled_at.
ALTER TABLE rooms ADD COLUMN early_open_minutes INTEGER DEFAULT 10;
ALTER TABLE rooms ADD COLUMN opened_at DATETIME;
