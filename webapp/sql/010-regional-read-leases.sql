-- Positive read leases survive application disconnects. A mutation takes a
-- shared lock until commit. A grant skips busy slots, and every query whose
-- result is cached starts after the grant. Hash collisions only cause waits.
BEGIN;
CREATE TABLE IF NOT EXISTS regional_read_slots (
  slot integer PRIMARY KEY,
  until_at timestamptz NOT NULL DEFAULT '-infinity'
);
INSERT INTO regional_read_slots(slot) SELECT generate_series(0,8193)
ON CONFLICT DO NOTHING;

CREATE OR REPLACE FUNCTION regional_slot(k text) RETURNS integer
LANGUAGE sql IMMUTABLE STRICT AS $$
  SELECT 2 + (hashtextextended(k,0) & 8191)::integer
$$;

CREATE OR REPLACE FUNCTION regional_grant(keys integer[], lifetime interval) RETURNS boolean
LANGUAGE plpgsql AS $$
DECLARE needed integer[]; locked integer[];
BEGIN
  SELECT array_agg(DISTINCT k ORDER BY k) INTO needed FROM unnest(keys) k WHERE k IS NOT NULL;
  SELECT array_agg(slot ORDER BY slot) INTO locked FROM
    (SELECT slot FROM regional_read_slots WHERE slot=ANY(needed)
     ORDER BY slot FOR UPDATE SKIP LOCKED) s;
  IF locked IS DISTINCT FROM needed THEN RETURN false; END IF;
  -- Losing a grant on a primary restart could let a writer commit while a
  -- remote process still holds it, even if other sessions use async commit.
  PERFORM set_config('synchronous_commit','on',true);
  UPDATE regional_read_slots SET until_at=greatest(until_at, clock_timestamp()+lifetime)
    WHERE slot=ANY(needed);
  RETURN true;
END $$;

CREATE OR REPLACE FUNCTION regional_friends(viewer text) RETURNS integer[]
LANGUAGE sql STABLE AS $$
  SELECT ARRAY(SELECT regional_slot('user:' || other_id) FROM (
    SELECT CASE WHEN user_low_id=viewer THEN user_high_id ELSE user_low_id END AS other_id
    FROM friendships WHERE user_low_id=viewer OR user_high_id=viewer
    ORDER BY other_id LIMIT 97) f ORDER BY 1)
$$;

CREATE OR REPLACE FUNCTION regional_feed_grant(token text, region_name text, ip_address text)
RETURNS TABLE(viewer text, leased boolean) LANGUAGE plpgsql AS $$
DECLARE friends integer[]; candidate text; granted boolean := false;
BEGIN
  SELECT s.user_id INTO candidate FROM login_sessions s JOIN users u ON u.id=s.user_id
    WHERE s.token_hash=token AND s.active AND u.status='active' AND u.region=region_name;
  IF candidate IS NULL THEN RETURN; END IF;
  friends:=regional_friends(candidate);
  IF cardinality(friends)<=96 THEN
    granted:=regional_grant(ARRAY[0,regional_slot('user:'||candidate),regional_slot('ip:'||ip_address)]
      || friends || CASE WHEN cardinality(friends)>0 THEN ARRAY[1] ELSE '{}'::integer[] END, interval '950 milliseconds');
  END IF;
  -- A writer may have committed between enumerating friends and locking.
  -- Recheck the graph under the viewer's lease. When the grant is unavailable,
  -- ordinary fresh auth is still sufficient for this request only.
  RETURN QUERY SELECT candidate,granted AND friends=regional_friends(candidate)
    FROM login_sessions s JOIN users u ON u.id=s.user_id
    WHERE s.user_id=candidate AND s.token_hash=token AND s.active AND u.status='active' AND u.region=region_name
    AND NOT EXISTS (SELECT 1 FROM ip_bans WHERE ip=ip_address AND banned_until>clock_timestamp());
END $$;

CREATE OR REPLACE FUNCTION regional_image_grant(image text, ip_address text)
RETURNS TABLE(secret_key_hash text,generation bigint,created_at timestamptz,media_type text)
LANGUAGE plpgsql AS $$
DECLARE owner_id text;
BEGIN
  SELECT p.author_id INTO owner_id FROM posts p WHERE p.image_id=image AND p.deleted_at IS NULL LIMIT 1;
  IF owner_id IS NULL OR NOT regional_grant(ARRAY[0,regional_slot('image:'||image),
    regional_slot('owner:'||owner_id),regional_slot('ip:'||ip_address)],interval '2 seconds')
    THEN RETURN; END IF;
  RETURN QUERY SELECT u.secret_key_hash,g.generation,i.created_at,i.media_type
    FROM image_objects i JOIN users u ON u.id=i.owner_user_id CROSS JOIN image_cache_generation g
    WHERE i.id=image AND u.id=owner_id AND u.status='active' AND EXISTS (
      SELECT 1 FROM posts p WHERE p.image_id=i.id AND p.author_id=u.id AND p.deleted_at IS NULL
    ) AND NOT EXISTS (SELECT 1 FROM ip_bans WHERE ip=ip_address AND banned_until>clock_timestamp());
END $$;

CREATE OR REPLACE FUNCTION regional_wait(keys integer[]) RETURNS void
LANGUAGE plpgsql AS $$
DECLARE deadline timestamptz;
BEGIN
  SELECT max(until_at) INTO deadline FROM (
    SELECT until_at FROM regional_read_slots WHERE slot=ANY(keys) ORDER BY slot FOR SHARE
  ) s;
  IF deadline>clock_timestamp() THEN
    PERFORM pg_sleep(greatest(0,extract(epoch FROM deadline-clock_timestamp())));
  END IF;
END $$;

-- All writes, including writes from older binaries, pass these gates. A writer
-- holds compatible SHARE locks, so unrelated application writes can proceed.
CREATE OR REPLACE FUNCTION regional_wait_row() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE keys integer[] := '{}'; col integer; val text; rowdata jsonb;
BEGIN
  -- Alternating namespace / column arguments identify both old and new keys.
  FOR i IN 1..2 LOOP
    IF (i=1 AND TG_OP='INSERT') OR (i=2 AND TG_OP='DELETE') THEN CONTINUE; END IF;
    IF i=1 THEN rowdata:=to_jsonb(OLD); ELSE rowdata:=to_jsonb(NEW); END IF;
    col:=0;
    WHILE col < TG_NARGS LOOP
      val:=rowdata->>TG_ARGV[col+1];
      keys:=array_append(keys, regional_slot(TG_ARGV[col]||val));
      col:=col+2;
    END LOOP;
  END LOOP;
  PERFORM regional_wait(keys);
  RETURN NULL;
END $$;

CREATE OR REPLACE FUNCTION regional_wait_global() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  PERFORM regional_wait(ARRAY[TG_ARGV[0]::integer]);
  RETURN NULL;
END $$;

-- AFTER row triggers acquire the guard before commit. The only instant that
-- matters to another client is when the mutation has succeeded.
CREATE OR REPLACE TRIGGER regional_users AFTER UPDATE OF id,username,display_name,bio,profile_image_id,region,account_type,status,secret_key_hash OR DELETE ON users
  FOR EACH ROW EXECUTE FUNCTION regional_wait_row('user:', 'id');
CREATE OR REPLACE TRIGGER regional_owner_changes AFTER UPDATE OF status,secret_key_hash,id OR DELETE ON users
  FOR EACH ROW EXECUTE FUNCTION regional_wait_row('owner:', 'id');
CREATE OR REPLACE TRIGGER regional_sessions AFTER UPDATE OR DELETE ON login_sessions
  FOR EACH ROW EXECUTE FUNCTION regional_wait_row('user:', 'user_id');
CREATE OR REPLACE TRIGGER regional_posts AFTER INSERT OR UPDATE OR DELETE ON posts
  FOR EACH ROW EXECUTE FUNCTION regional_wait_row('user:', 'author_id');
CREATE OR REPLACE TRIGGER regional_post_images AFTER UPDATE OF deleted_at,image_id,author_id OR DELETE ON posts
  FOR EACH ROW EXECUTE FUNCTION regional_wait_row('image:', 'image_id');
CREATE OR REPLACE TRIGGER regional_friends AFTER INSERT OR UPDATE OR DELETE ON friendships
  FOR EACH ROW EXECUTE FUNCTION regional_wait_row('user:', 'user_low_id','user:', 'user_high_id');
CREATE OR REPLACE TRIGGER regional_blocks AFTER INSERT OR UPDATE OR DELETE ON block_relations
  FOR EACH ROW EXECUTE FUNCTION regional_wait_row('user:', 'blocker_id','user:', 'blocked_id');
CREATE OR REPLACE TRIGGER regional_ip AFTER INSERT OR UPDATE OR DELETE ON ip_bans
  FOR EACH ROW EXECUTE FUNCTION regional_wait_row('ip:', 'ip');
-- Do not convert the potentially large image body to jsonb to find its ID.
CREATE OR REPLACE FUNCTION regional_wait_image() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  PERFORM regional_wait(ARRAY[regional_slot('image:'||OLD.id),regional_slot('image:'||NEW.id)]);
  RETURN NULL;
END $$;
CREATE OR REPLACE TRIGGER regional_image_changes AFTER UPDATE OR DELETE ON image_objects
  FOR EACH ROW EXECUTE FUNCTION regional_wait_image();
CREATE OR REPLACE TRIGGER regional_window AFTER INSERT OR UPDATE OR DELETE ON posting_windows
  FOR EACH STATEMENT EXECUTE FUNCTION regional_wait_global('1');
CREATE OR REPLACE TRIGGER regional_reset BEFORE UPDATE OR DELETE OR TRUNCATE ON image_cache_generation
  FOR EACH STATEMENT EXECUTE FUNCTION regional_wait_global('0');
-- reset.sql changes session_replication_role and truncates data in the same
-- transaction as the epoch update. This gate must fire in replica mode too.
ALTER TABLE image_cache_generation ENABLE ALWAYS TRIGGER regional_reset;
DO $$
DECLARE tab text;
BEGIN
  FOREACH tab IN ARRAY ARRAY['users','posts','image_objects','friendships','block_relations',
    'login_sessions','ip_bans','posting_windows'] LOOP
    EXECUTE format('CREATE OR REPLACE TRIGGER regional_truncate BEFORE TRUNCATE ON %I
      FOR EACH STATEMENT EXECUTE FUNCTION regional_wait_global(''0'')',tab);
  END LOOP;
  -- A direct TRUNCATE of an image partition does not fire the parent's
  -- statement trigger. The normal replica-mode reset is covered by its epoch.
  FOR tab IN SELECT relid::regclass::text FROM pg_partition_tree('image_objects') WHERE isleaf LOOP
    EXECUTE format('CREATE OR REPLACE TRIGGER regional_truncate BEFORE TRUNCATE ON %s
      FOR EACH STATEMENT EXECUTE FUNCTION regional_wait_global(''0'')',tab);
  END LOOP;
END $$;
COMMIT;
