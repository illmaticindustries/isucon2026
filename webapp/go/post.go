package isureal

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"

	"github.com/jmoiron/sqlx"
)

type PostLocation struct {
	Kind      string   `json:"kind" db:"location_kind"`
	Latitude  *float64 `json:"latitude,omitempty" db:"latitude"`
	Longitude *float64 `json:"longitude,omitempty" db:"longitude"`
	Locality  *string  `json:"locality,omitempty" db:"locality"`
}

func (location PostLocation) valid() bool {
	switch location.Kind {
	case "none":
		return location.Latitude == nil && location.Longitude == nil && location.Locality == nil
	case "exact":
		return location.Latitude != nil && location.Longitude != nil && location.Locality == nil &&
			*location.Latitude >= -90 && *location.Latitude <= 90 && *location.Longitude >= -180 && *location.Longitude <= 180
	case "approximate":
		return location.Latitude == nil && location.Longitude == nil && location.Locality != nil && validUserText(*location.Locality, 1, 100)
	default:
		return false
	}
}

type Post struct {
	ID              string `db:"id"`
	AuthorID        string `db:"author_id"`
	PostingWindowID string `db:"posting_window_id"`
	ImageID         string `db:"image_id"`
	Caption         string `db:"caption"`
	Visibility      string `db:"visibility"`
	PostLocation
	PostedAt       time.Time  `db:"posted_at"`
	OnTimeDeadline time.Time  `db:"on_time_deadline"`
	WindowEndedAt  *time.Time `db:"window_ended_at"`
}

const postColumns = `p.id, p.author_id, p.posting_window_id, p.image_id, p.caption, p.visibility,
  p.location_kind, p.latitude, p.longitude, p.locality, p.posted_at,
  w.on_time_deadline, w.ended_at AS window_ended_at`

func getPost(ctx context.Context, db sqlx.QueryerContext, postID string) (Post, error) {
	var post Post
	err := sqlx.GetContext(ctx, db, &post, `
  SELECT `+postColumns+`
  FROM posts p JOIN posting_windows w ON w.id = p.posting_window_id
  WHERE p.id = $1 AND p.deleted_at IS NULL
 `, postID)
	return post, err
}

type PostWithAuthor struct {
	Post
	Author User `db:"author"`
}

func selectPostsWithAuthor(ctx context.Context, db sqlx.QueryerContext, conditions string, args ...any) ([]PostWithAuthor, error) {
	var posts []PostWithAuthor
	if err := sqlx.SelectContext(ctx, db, &posts, `
  SELECT `+postColumns+`, `+userColumnsAs("u", "author")+`
  FROM posts p
  JOIN posting_windows w ON w.id = p.posting_window_id
  JOIN users u ON u.id = p.author_id AND u.status = 'active'
  WHERE p.deleted_at IS NULL `+conditions, args...); err != nil {
		return nil, err
	}
	for i := range posts {
		if err := posts[i].Author.setImageKey(); err != nil {
			return nil, err
		}
	}
	return posts, nil
}

func getPostWithAuthor(ctx context.Context, db sqlx.QueryerContext, postID string) (PostWithAuthor, error) {
	posts, err := selectPostsWithAuthor(ctx, db, `AND p.id = $1`, postID)
	if err != nil {
		return PostWithAuthor{}, err
	}
	if len(posts) == 0 {
		return PostWithAuthor{}, sql.ErrNoRows
	}
	return posts[0], nil
}

type viewerContext struct {
	User
	HasPostedToday   bool
	Friends          map[string]bool
	FriendsOfFriends map[string]bool
	MutualFriends    map[string]int
	Blocked          map[string]bool
}

func loadViewerContext(ctx context.Context, db sqlx.QueryerContext, viewerID string) (viewerContext, error) {
	viewer := viewerContext{Friends: map[string]bool{}, FriendsOfFriends: map[string]bool{}, MutualFriends: map[string]int{}, Blocked: map[string]bool{}}
	// All viewer lookups are fetched in one statement. A region that reaches the
	// shared database over an inter-region link pays a round trip per query, so
	// collapsing the fixed set of lookups into one matters there.
	var row struct {
		User
		HasPostedToday bool   `db:"has_posted_today"`
		Friends        []byte `db:"friends"`
		Blocked        []byte `db:"blocked"`
		Friendships    []byte `db:"friendships"`
	}
	if err := sqlx.GetContext(ctx, db, &row, `
  WITH me AS (
    SELECT id, username, secret_key_hash, display_name, bio, profile_image_id, region, account_type, status
    FROM users WHERE id = $1 AND status = 'active'
  ),
  friends AS (
    SELECT CASE WHEN f.user_low_id = $1 THEN f.user_high_id ELSE f.user_low_id END AS id
    FROM friendships f WHERE f.user_low_id = $1 OR f.user_high_id = $1
  ),
  blocked AS (
    SELECT CASE WHEN b.blocker_id = $1 THEN b.blocked_id ELSE b.blocker_id END AS id
    FROM block_relations b WHERE b.blocker_id = $1 OR b.blocked_id = $1
  ),
  friends_of_friends AS (
    SELECT f.user_low_id, f.user_high_id
    FROM friendships f
    WHERE f.user_low_id IN (SELECT id FROM friends) OR f.user_high_id IN (SELECT id FROM friends)
  )
  SELECT me.id, me.username, me.secret_key_hash, me.display_name, me.bio, me.profile_image_id,
         me.region, me.account_type, me.status,
    (SELECT EXISTS(
       SELECT 1 FROM posts p
       JOIN posting_windows posted ON posted.id = p.posting_window_id
       JOIN posting_windows current ON current.region = me.region AND current.ended_at IS NULL
                                   AND current.local_date = posted.local_date
       WHERE p.author_id = me.id AND p.deleted_at IS NULL
     )) AS has_posted_today,
    (SELECT COALESCE(json_agg(id), '[]'::json)::text FROM friends) AS friends,
    (SELECT COALESCE(json_agg(id), '[]'::json)::text FROM blocked) AS blocked,
    (SELECT COALESCE(json_agg(json_build_array(user_low_id, user_high_id)), '[]'::json)::text
       FROM friends_of_friends) AS friendships
  FROM me
  `, viewerID); err != nil {
		return viewer, err
	}
	viewer.User = row.User
	if err := viewer.setImageKey(); err != nil {
		return viewer, err
	}
	viewer.HasPostedToday = row.HasPostedToday
	var blocked []string
	if err := json.Unmarshal(row.Blocked, &blocked); err != nil {
		return viewer, err
	}
	for _, id := range blocked {
		viewer.Blocked[id] = true
	}
	var friends []string
	if err := json.Unmarshal(row.Friends, &friends); err != nil {
		return viewer, err
	}
	for _, id := range friends {
		viewer.Friends[id] = true
	}
	var friendships [][2]string
	if err := json.Unmarshal(row.Friendships, &friendships); err != nil {
		return viewer, err
	}
	for _, friendship := range friendships {
		low, high := friendship[0], friendship[1]
		if viewer.Friends[low] {
			viewer.FriendsOfFriends[high] = true
			if high != viewer.ID {
				viewer.MutualFriends[high]++
			}
		}
		if viewer.Friends[high] {
			viewer.FriendsOfFriends[low] = true
			if low != viewer.ID {
				viewer.MutualFriends[low]++
			}
		}
	}
	return viewer, nil
}

func (viewer viewerContext) canView(post Post) bool {
	if post.AuthorID == viewer.ID {
		return true
	}
	if post.WindowEndedAt != nil || !viewer.HasPostedToday || viewer.Blocked[post.AuthorID] {
		return false
	}
	switch post.Visibility {
	case "public":
		return true
	case "friends":
		return viewer.Friends[post.AuthorID]
	case "friends_of_friends":
		return viewer.Friends[post.AuthorID] || viewer.FriendsOfFriends[post.AuthorID]
	default:
		return false
	}
}

func canViewPost(ctx context.Context, db sqlx.QueryerContext, viewerID, postID string) (bool, error) {
	return postAccessAllowed(ctx, db, viewerID, postID, false)
}

func getFriendIDs(ctx context.Context, db sqlx.QueryerContext, userID string) ([]string, error) {
	var friendships []struct {
		UserLowID  string `db:"user_low_id"`
		UserHighID string `db:"user_high_id"`
	}
	if err := sqlx.SelectContext(ctx, db, &friendships, `SELECT user_low_id, user_high_id FROM friendships WHERE user_low_id = $1 OR user_high_id = $1`, userID); err != nil {
		return nil, err
	}
	var ids []string
	for _, friendship := range friendships {
		if friendship.UserLowID == userID {
			ids = append(ids, friendship.UserHighID)
		} else {
			ids = append(ids, friendship.UserLowID)
		}
	}
	return ids, nil
}

func getBlockedUserIDs(ctx context.Context, db sqlx.QueryerContext, userID string) (map[string]bool, error) {
	var ids []string
	if err := sqlx.SelectContext(ctx, db, &ids, `
  SELECT CASE WHEN blocker_id = $1 THEN blocked_id ELSE blocker_id END
  FROM block_relations WHERE blocker_id = $1 OR blocked_id = $1
 `, userID); err != nil {
		return nil, err
	}
	blocked := make(map[string]bool, len(ids))
	for _, id := range ids {
		blocked[id] = true
	}
	return blocked, nil
}
