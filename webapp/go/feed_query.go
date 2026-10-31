package isureal

import (
	"context"

	"github.com/jmoiron/sqlx"
)

// Chronological feeds filter authorization before taking the page. MATERIALIZED
// bounds the detail/author fetch to 51 rows in the same statement snapshot.
// Friend-of-friend traversal is needed only for following a non-friend's
// friends_of_friends post; ordinary friends/public pages do not expand a graph.
func selectChronologicalFeed(ctx context.Context, db sqlx.QueryerContext, viewerID string, input getFeedRequest) ([]PostWithAuthor, error) {
	predicate := ""
	openWindow := `p.posting_window_id = ANY(ARRAY(SELECT id FROM posting_windows WHERE ended_at IS NULL))`
	switch input.Feed {
	case "friends":
		// Compute the friend IDs once so PostgreSQL can probe the live posts
		// index by both open window and author, even with a prepared plan.
		predicate = `p.author_id = ANY(ARRAY(SELECT id FROM viewer_friends))
    AND p.visibility IN ('public', 'friends', 'friends_of_friends')`
	case "public":
		// Public has no author restriction. Let the planner join the open windows directly.
		openWindow = `EXISTS (SELECT 1 FROM posting_windows w WHERE w.id=p.posting_window_id AND w.ended_at IS NULL)`
		predicate = `p.visibility = 'public'`
	case "following":
		predicate = `EXISTS (SELECT 1 FROM follows f WHERE f.follower_id=$1 AND f.followee_id=p.author_id)
    AND (p.visibility='public'
      OR (p.visibility IN ('friends','friends_of_friends') AND p.author_id IN (SELECT id FROM viewer_friends))
      OR (p.visibility='friends_of_friends' AND EXISTS (
        SELECT 1 FROM friendships f2
        WHERE (f2.user_low_id=p.author_id AND f2.user_high_id IN (SELECT id FROM viewer_friends))
           OR (f2.user_high_id=p.author_id AND f2.user_low_id IN (SELECT id FROM viewer_friends)))))`
	default:
		return nil, NewAPIError(400, "invalid_post")
	}
	var posts []PostWithAuthor
	err := sqlx.SelectContext(ctx, db, &posts, `
 WITH viewer AS MATERIALIZED (
   SELECT u.id, EXISTS (
     SELECT 1 FROM posts mine
     JOIN posting_windows posted ON posted.id=mine.posting_window_id
     JOIN posting_windows current ON current.region=u.region AND current.ended_at IS NULL
                                  AND current.local_date=posted.local_date
     WHERE mine.author_id=u.id AND mine.deleted_at IS NULL
   ) AS has_posted
   FROM users u WHERE u.id=$1 AND u.status='active'
 ), viewer_friends AS MATERIALIZED (
   SELECT CASE WHEN user_low_id=$1 THEN user_high_id ELSE user_low_id END AS id
   FROM friendships WHERE user_low_id=$1 OR user_high_id=$1
 ), candidates AS MATERIALIZED (
   SELECT p.id, p.posted_at
   FROM posts p
   JOIN users author ON author.id=p.author_id AND author.status='active'
   CROSS JOIN viewer v
   WHERE v.has_posted AND p.author_id<>$1 AND p.deleted_at IS NULL
     AND `+openWindow+`
     AND ($2::timestamptz IS NULL OR (p.posted_at,p.id)<($2,$3))
     AND NOT EXISTS (
       SELECT 1 FROM block_relations b
       WHERE (b.blocker_id=$1 AND b.blocked_id=p.author_id)
          OR (b.blocked_id=$1 AND b.blocker_id=p.author_id)
     ) AND (`+predicate+`)
   ORDER BY p.posted_at DESC, p.id DESC LIMIT 51
 )
 SELECT `+postColumns+`, `+userColumnsAs("u", "author")+`
 FROM candidates c
 JOIN posts p ON p.id=c.id
 JOIN posting_windows w ON w.id=p.posting_window_id
 JOIN users u ON u.id=p.author_id
 ORDER BY c.posted_at DESC, c.id DESC
 `, viewerID, input.BeforePostedAt, input.BeforeID)
	return posts, err
}
