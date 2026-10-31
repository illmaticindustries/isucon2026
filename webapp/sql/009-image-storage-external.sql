-- Store image bytes without trying to compress them. JPEG does not compress
-- (0 of 1,555 stored images shrank), yet every INSERT ran pglz over ~415KB on a
-- 2-core database. Only the storage strategy changes: existing rows, reads and
-- API responses are unchanged, and new rows are still TOASTed out of line.
-- SET STORAGE recurses to every partition and briefly takes ACCESS EXCLUSIVE,
-- so run it only when some relation is not EXTERNAL yet, and give up instead of
-- queueing all image reads behind a long-running transaction.
SET lock_timeout = '3s';
DO $$
BEGIN
  IF EXISTS (
    SELECT 1 FROM pg_attribute a
    WHERE a.attname = 'data' AND a.attstorage <> 'e'
      AND (a.attrelid = 'image_objects'::regclass OR a.attrelid IN (
        SELECT inhrelid FROM pg_inherits WHERE inhparent = 'image_objects'::regclass))
  ) THEN
    ALTER TABLE image_objects ALTER COLUMN data SET STORAGE EXTERNAL;
  END IF;
END $$;
