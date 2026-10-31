package isureal

import (
	"context"

	"github.com/jmoiron/sqlx"
)

// Point lookups only need the relationship to this author. Evaluate against one
// primary snapshot, including the viewer's current local date after a move.
// $3 selects comment permission, which never extends to a second-degree friend.
const permittedPostFrom = `
 FROM posts p
 JOIN posting_windows w ON w.id=p.posting_window_id
 JOIN users u ON u.id=p.author_id AND u.status='active'
 JOIN users v ON v.id=$2 AND v.status='active'
 WHERE p.id=$1 AND p.deleted_at IS NULL AND CASE
   WHEN p.author_id=v.id AND NOT $3::boolean THEN true
   WHEN w.ended_at IS NOT NULL OR NOT EXISTS (
     SELECT 1 FROM posts mine
     JOIN posting_windows posted ON posted.id=mine.posting_window_id
     JOIN posting_windows current ON current.region=v.region AND current.ended_at IS NULL
                                  AND current.local_date=posted.local_date
     WHERE mine.author_id=v.id AND mine.deleted_at IS NULL
   ) THEN false
   WHEN p.author_id=v.id THEN true
   WHEN EXISTS (SELECT 1 FROM block_relations b
                WHERE (b.blocker_id=v.id AND b.blocked_id=u.id)
                   OR (b.blocker_id=u.id AND b.blocked_id=v.id)) THEN false
   WHEN p.visibility='public' THEN true
   WHEN p.visibility NOT IN ('friends','friends_of_friends') THEN false
   WHEN EXISTS (SELECT 1 FROM friendships f
                WHERE (f.user_low_id=v.id AND f.user_high_id=u.id)
                   OR (f.user_low_id=u.id AND f.user_high_id=v.id)) THEN true
   WHEN p.visibility='friends_of_friends' AND NOT $3::boolean THEN EXISTS (
     SELECT 1 FROM friendships f
     WHERE (f.user_low_id=v.id OR f.user_high_id=v.id)
       AND EXISTS (SELECT 1 FROM friendships a
                   WHERE (a.user_low_id=u.id AND a.user_high_id=CASE WHEN f.user_low_id=v.id THEN f.user_high_id ELSE f.user_low_id END)
                      OR (a.user_high_id=u.id AND a.user_low_id=CASE WHEN f.user_low_id=v.id THEN f.user_high_id ELSE f.user_low_id END))
   )
   ELSE false END`

func getVisiblePost(ctx context.Context, db sqlx.QueryerContext, viewerID, postID string) (PostWithAuthor, error) {
	var row PostWithAuthor
	err := sqlx.GetContext(ctx, db, &row, `SELECT `+postColumns+`, `+userColumnsAs("u", "author")+permittedPostFrom, postID, viewerID, false)
	if err == nil {
		err = row.Author.setImageKey()
	}
	return row, err
}

func postAccessAllowed(ctx context.Context, db sqlx.QueryerContext, viewerID, postID string, comment bool) (bool, error) {
	var allowed bool
	err := sqlx.GetContext(ctx, db, &allowed, `SELECT EXISTS (SELECT 1`+permittedPostFrom+`)`, postID, viewerID, comment)
	return allowed, err
}
