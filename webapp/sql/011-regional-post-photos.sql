BEGIN;
SET LOCAL lock_timeout = '3s';

-- Enable writes only after every running reader understands regional photos.
ALTER TABLE image_cache_generation ADD COLUMN IF NOT EXISTS regional_post_photos boolean NOT NULL DEFAULT false;
-- One private peer credential shared by all processes, including after restart.
CREATE TABLE IF NOT EXISTS regional_photo_peer_key (
    singleton boolean PRIMARY KEY DEFAULT true CHECK(singleton),
    key_bytes bytea NOT NULL CHECK(octet_length(key_bytes)=32)
);
REVOKE ALL ON regional_photo_peer_key FROM PUBLIC;
INSERT INTO regional_photo_peer_key VALUES(true, uuid_send(gen_random_uuid()) || uuid_send(gen_random_uuid()))
    ON CONFLICT(singleton) DO NOTHING;
CREATE TABLE IF NOT EXISTS regional_post_photos (
    image_id varchar(36) NOT NULL,
    created_at timestamptz NOT NULL,
    region text NOT NULL CHECK (region IN ('asia','europe','america')),
    PRIMARY KEY (image_id, created_at)
);
CREATE INDEX IF NOT EXISTS regional_post_photos_region_idx ON regional_post_photos (region);

-- A VOLATILE function takes a new READ COMMITTED snapshot for each statement.
-- The slot check must see the winner after waiting for the author's row lock.
CREATE OR REPLACE FUNCTION insert_regional_photo_post(
    p_id text, p_image text, p_author text, p_caption text, p_visibility text,
    p_kind text, p_lat numeric, p_lon numeric, p_locality text, p_data bytea,
    p_region text, p_epoch bigint
) RETURNS TABLE(author_row users, window_id varchar, at_time timestamptz, deadline timestamptz, epoch bigint)
LANGUAGE plpgsql VOLATILE AS $$
DECLARE
    win posting_windows%ROWTYPE;
    use_files boolean;
    active_count int;
    deleted_count int;
BEGIN
    SELECT g.generation, g.regional_post_photos INTO STRICT epoch, use_files
        FROM image_cache_generation g WHERE singleton FOR SHARE;
    IF p_region <> '' AND (NOT use_files OR p_epoch <> epoch) THEN
        RAISE EXCEPTION USING ERRCODE='PZ001', MESSAGE='regional photo mode changed';
    END IF;
    SELECT * INTO STRICT author_row FROM users WHERE id=p_author AND status='active' FOR NO KEY UPDATE;
    SELECT * INTO win FROM posting_windows WHERE region=author_row.region AND ended_at IS NULL FOR SHARE;
    IF NOT FOUND THEN RETURN; END IF;
    SELECT count(*) FILTER (WHERE p.deleted_at IS NULL), count(*) FILTER (WHERE p.deleted_at IS NOT NULL)
        INTO active_count, deleted_count FROM posts p JOIN posting_windows w ON w.id=p.posting_window_id
        WHERE p.author_id=p_author AND w.local_date=win.local_date;
    at_time := clock_timestamp();
    IF active_count <> 0 OR deleted_count >= 2 OR at_time < win.starts_at THEN RETURN; END IF;

    INSERT INTO image_objects(id,owner_user_id,data,media_type,created_at)
        VALUES(p_image,p_author,p_data,'image/jpeg',at_time);
    IF p_region <> '' THEN
        INSERT INTO regional_post_photos(image_id,created_at,region) VALUES(p_image,at_time,p_region);
    END IF;
    INSERT INTO posts(id,author_id,posting_window_id,image_id,caption,visibility,
                      location_kind,latitude,longitude,locality,posted_at)
        VALUES(p_id,p_author,win.id,p_image,p_caption,p_visibility,p_kind,p_lat,p_lon,p_locality,at_time);
    window_id := win.id;
    deadline := win.on_time_deadline;
    RETURN NEXT;
END;
$$;

COMMIT;
