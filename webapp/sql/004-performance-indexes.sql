-- Run with psql -X --set=ON_ERROR_STOP=1 --file=004-performance-indexes.sql.
-- CREATE INDEX CONCURRENTLY must run outside a transaction block.
-- reset.sql preserves these indexes across benchmark initialization.
\set ON_ERROR_STOP on

-- Image validity checks must find the live reference without scanning history.
CREATE INDEX CONCURRENTLY IF NOT EXISTS realmojis_active_image_reactor
  ON realmojis (image_id, reactor_id) INCLUDE (post_id)
  WHERE replaced_at IS NULL AND deleted_at IS NULL;

CREATE INDEX CONCURRENTLY IF NOT EXISTS posts_active_image_author
  ON posts (image_id, author_id)
  WHERE deleted_at IS NULL;

-- Feed pages probe open windows and (for friends) the viewer's direct friends.
CREATE INDEX CONCURRENTLY IF NOT EXISTS posts_live_window_author
  ON posts (posting_window_id, author_id)
  WHERE deleted_at IS NULL;

-- The existing (user_low_id, user_high_id) index covers the other direction.
CREATE INDEX CONCURRENTLY IF NOT EXISTS friendships_high_low
  ON friendships (user_high_id, user_low_id);

CREATE INDEX CONCURRENTLY IF NOT EXISTS block_relations_blocked_blocker
  ON block_relations (blocked_id, blocker_id);

-- Preserve existing username uniqueness/locking behavior; only add lookup indexes.
CREATE INDEX CONCURRENTLY IF NOT EXISTS users_username
  ON users (username);

CREATE INDEX CONCURRENTLY IF NOT EXISTS login_sessions_active_token
  ON login_sessions (token_hash) INCLUDE (user_id, created_at)
  WHERE active = true;

CREATE INDEX CONCURRENTLY IF NOT EXISTS login_sessions_active_user
  ON login_sessions (user_id)
  WHERE active = true;

-- Both directions of the pending pair lookup can use this same composite index.
CREATE INDEX CONCURRENTLY IF NOT EXISTS friend_requests_pending_pair
  ON friend_requests (requester_id, recipient_id) INCLUDE (id)
  WHERE status = 'pending';
