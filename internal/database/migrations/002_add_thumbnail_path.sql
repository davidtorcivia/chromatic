-- Add thumbnail_path column to files table
-- This column stores the path to generated image thumbnails

ALTER TABLE files ADD COLUMN thumbnail_path TEXT;
