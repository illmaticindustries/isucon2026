\set ON_ERROR_STOP on
SET default_transaction_read_only = on;

SELECT c.relname AS index_name, i.indisvalid, i.indisready,
       pg_get_indexdef(c.oid) AS definition
FROM pg_index i JOIN pg_class c ON c.oid = i.indexrelid
WHERE c.relnamespace = 'public'::regnamespace
  AND c.relname IN (
    'realmojis_active_image_reactor', 'posts_active_image_author',
    'friendships_high_low', 'block_relations_blocked_blocker',
    'users_username', 'login_sessions_active_token', 'login_sessions_active_user',
    'friend_requests_pending_pair'
  )
ORDER BY c.relname;

-- Fail deployment verification if an index is absent or a concurrent build failed.
DO $$
BEGIN
  IF EXISTS (
    SELECT 1 FROM (VALUES
      ('realmojis_active_image_reactor'), ('posts_active_image_author'),
      ('friendships_high_low'), ('block_relations_blocked_blocker'),
      ('users_username'), ('login_sessions_active_token'), ('login_sessions_active_user'),
      ('friend_requests_pending_pair')
    ) expected(name)
    LEFT JOIN pg_class c ON c.relname = expected.name
      AND c.relnamespace = 'public'::regnamespace
    LEFT JOIN pg_index i ON i.indexrelid = c.oid
    WHERE i.indisvalid IS DISTINCT FROM true OR i.indisready IS DISTINCT FROM true
  ) THEN
    RAISE EXCEPTION 'Performance indexes missing or invalid; inspect concurrent index build before retrying';
  END IF;
END
$$;
