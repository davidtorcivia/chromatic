-- Per-room watermark placement/sizing and a per-room participant limit.
--
-- watermark_pos_x / watermark_pos_y store the CENTER of the watermark as a
-- fraction (0-1) of the video width/height. NULL = legacy built-in placement
-- (text in the bottom-right corner, logo at watermark_logo_position).
--
-- watermark_scale multiplies the base text font size / logo size
-- (clamped to 0.25-3.0 by the handlers; 1.0 = current size).
--
-- max_participants overrides the global MaxParticipantsPerRoom cap when set
-- (validated 1-100 by the handlers). NULL = use the global default.
ALTER TABLE rooms ADD COLUMN watermark_pos_x REAL;
ALTER TABLE rooms ADD COLUMN watermark_pos_y REAL;
ALTER TABLE rooms ADD COLUMN watermark_scale REAL DEFAULT 1.0;
ALTER TABLE rooms ADD COLUMN max_participants INTEGER;
