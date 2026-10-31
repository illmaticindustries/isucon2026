package isureal

import (
	"context"
	"github.com/jmoiron/sqlx"
)

// Preserve the previous five-statement implementation as an independent oracle.
func referenceViewerContext(ctx context.Context, db sqlx.QueryerContext, viewerID string) (viewerContext, error) {
	viewer := viewerContext{Friends: map[string]bool{}, FriendsOfFriends: map[string]bool{}, MutualFriends: map[string]int{}}
	var err error
	if viewer.User, err = getUser(ctx, db, viewerID); err != nil {
		return viewer, err
	}
	if err := sqlx.GetContext(ctx, db, &viewer.HasPostedToday, `
  SELECT EXISTS(
    SELECT 1 FROM posts p
    JOIN posting_windows posted ON posted.id = p.posting_window_id
    JOIN posting_windows current ON current.region = $2 AND current.ended_at IS NULL
                                AND current.local_date = posted.local_date
    WHERE p.author_id = $1 AND p.deleted_at IS NULL
  )
 `, viewer.ID, viewer.Region); err != nil {
		return viewer, err
	}
	if viewer.Blocked, err = getBlockedUserIDs(ctx, db, viewer.ID); err != nil {
		return viewer, err
	}
	friends, err := getFriendIDs(ctx, db, viewer.ID)
	if err != nil {
		return viewer, err
	}
	if len(friends) == 0 {
		return viewer, nil
	}
	for _, id := range friends {
		viewer.Friends[id] = true
	}
	query, args, err := sqlx.In(`SELECT user_low_id, user_high_id FROM friendships WHERE user_low_id IN (?) OR user_high_id IN (?)`, friends, friends)
	if err != nil {
		return viewer, err
	}
	var friendships []struct {
		UserLowID  string `db:"user_low_id"`
		UserHighID string `db:"user_high_id"`
	}
	if err := sqlx.SelectContext(ctx, db, &friendships, sqlx.Rebind(sqlx.DOLLAR, query), args...); err != nil {
		return viewer, err
	}
	for _, friendship := range friendships {
		if viewer.Friends[friendship.UserLowID] {
			viewer.FriendsOfFriends[friendship.UserHighID] = true
			if friendship.UserHighID != viewer.ID {
				viewer.MutualFriends[friendship.UserHighID]++
			}
		}
		if viewer.Friends[friendship.UserHighID] {
			viewer.FriendsOfFriends[friendship.UserLowID] = true
			if friendship.UserLowID != viewer.ID {
				viewer.MutualFriends[friendship.UserLowID]++
			}
		}
	}
	return viewer, nil
}
