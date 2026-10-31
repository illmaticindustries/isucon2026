-- A derived search index. The source values make stale entries detectable even
-- when an older application updates users during a rollback.
ALTER TABLE users ADD COLUMN IF NOT EXISTS search_username_source text;
ALTER TABLE users ADD COLUMN IF NOT EXISTS search_display_name_source text;
ALTER TABLE users ADD COLUMN IF NOT EXISTS search_username_normalized text;
ALTER TABLE users ADD COLUMN IF NOT EXISTS search_display_name_normalized text;
