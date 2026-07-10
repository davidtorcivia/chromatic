-- rooms.slug is declared UNIQUE (001_initial.sql), which already creates an
-- implicit index; idx_rooms_slug duplicated it, so every room insert/update
-- maintained two identical b-trees for zero read benefit.
DROP INDEX IF EXISTS idx_rooms_slug;
