-- Distinguish frame grabs from ordinary uploads so serve-time watermarking can
-- key off provenance instead of the filename. The value is set by the route the
-- upload arrived on (POST /api/rooms/{slug}/grab), never by a client field, so
-- a client cannot opt its capture out of the stamp.
ALTER TABLE files ADD COLUMN origin TEXT NOT NULL DEFAULT 'upload';
-- 'upload' | 'frame-grab'
