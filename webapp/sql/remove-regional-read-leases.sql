-- Apply using make remove-regional-read-leases after restoring the application.
-- Wait out the last grant while preventing new ones. Remove only the derived
-- coordination state and triggers installed by 010-regional-read-leases.sql.
BEGIN;
SET LOCAL lock_timeout='3s';
DO $$
DECLARE rec record;
BEGIN
  IF to_regclass('regional_read_slots') IS NULL THEN RETURN; END IF;
  LOCK TABLE regional_read_slots IN ACCESS EXCLUSIVE MODE;
  PERFORM pg_sleep(coalesce((SELECT greatest(0,extract(epoch FROM max(until_at)-clock_timestamp()))
    FROM regional_read_slots WHERE until_at>clock_timestamp()),0));
  FOR rec IN SELECT t.tgname,t.tgrelid::regclass AS tbl FROM pg_trigger t
    JOIN pg_proc p ON p.oid=t.tgfoid
    WHERE p.pronamespace=current_schema()::regnamespace AND t.tgparentid=0
      AND p.proname IN ('regional_wait_row','regional_wait_image','regional_wait_global')
  LOOP
    EXECUTE format('DROP TRIGGER %I ON %s',rec.tgname,rec.tbl);
  END LOOP;
END $$;
DROP FUNCTION IF EXISTS regional_feed_grant(text,text,text),regional_image_grant(text,text);
DROP FUNCTION IF EXISTS regional_wait_row(),regional_wait_image(),regional_wait_global();
DROP FUNCTION IF EXISTS regional_friends(text),regional_grant(integer[],interval),regional_wait(integer[]),regional_slot(text);
DROP TABLE IF EXISTS regional_read_slots;
COMMIT;
