-- image_objects is partitioned by created_at, so a lookup by id alone opens and
-- locks every partition and index (81 relation locks), contending on the lock
-- manager. This unpartitioned map lets readers find created_at first and prune
-- to one partition. Every insert path is covered by the trigger; readers fall
-- back to the id-only lookup when an id is missing, so stale or absent rows
-- never change results.
CREATE TABLE IF NOT EXISTS image_object_index (
    id varchar(36) PRIMARY KEY,
    created_at timestamptz NOT NULL
);

CREATE OR REPLACE FUNCTION image_object_index_insert() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    INSERT INTO image_object_index (id, created_at) VALUES (NEW.id, NEW.created_at)
    ON CONFLICT (id) DO UPDATE SET created_at = EXCLUDED.created_at;
    RETURN NULL;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_trigger
        WHERE tgrelid = 'image_objects'::regclass AND tgname = 'image_object_index_insert'
    ) THEN
        CREATE TRIGGER image_object_index_insert AFTER INSERT ON image_objects
        FOR EACH ROW EXECUTE FUNCTION image_object_index_insert();
    END IF;
END
$$;

-- Backfill once; later inserts are maintained by the trigger.
INSERT INTO image_object_index (id, created_at)
SELECT id, created_at FROM image_objects
WHERE NOT EXISTS (SELECT 1 FROM image_object_index)
ON CONFLICT (id) DO NOTHING;
