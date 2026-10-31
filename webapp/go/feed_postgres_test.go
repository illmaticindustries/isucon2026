package isureal

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http/httptest"
	"net/url"
	"os"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/jmoiron/sqlx"
	"github.com/labstack/echo/v5"
)

// Reference keeps the former full-scan/Go-authorization algorithm, independently
// of the SQL predicate under test. Real PostgreSQL exercises CTEs, pgx bindings,
// NULL timestamps, and ordering. TEMP tables cannot change application data.
func referenceFeedPage(ctx context.Context, db *sqlx.DB, viewerID string, input getFeedRequest) (getFeedResponse, error) {
	empty := getFeedResponse{Items: []feedItemResponse{}}
	viewer, err := loadViewerContext(ctx, db, viewerID)
	if errors.Is(err, sql.ErrNoRows) {
		return empty, nil
	}
	if err != nil {
		return empty, err
	}
	following := map[string]bool{}
	if input.Feed == "following" {
		var ids []string
		if err := db.SelectContext(ctx, &ids, `SELECT followee_id FROM follows WHERE follower_id=$1`, viewerID); err != nil {
			return empty, err
		}
		for _, id := range ids {
			following[id] = true
		}
	}
	ranked := input.Feed == "friends-of-friends"
	before, beforeID := input.BeforePostedAt, input.BeforeID
	if ranked {
		before, beforeID = nil, ""
	}
	posts, err := selectPostsWithAuthor(ctx, db, `AND p.author_id<>$1 AND w.ended_at IS NULL
 AND ($2::timestamptz IS NULL OR (p.posted_at,p.id)<($2,$3)) ORDER BY p.posted_at DESC,p.id DESC`, viewerID, before, beforeID)
	if err != nil {
		return empty, err
	}
	var candidates []PostWithAuthor
	for _, post := range posts {
		if viewer.canView(post.Post) && matchesFeed(viewer, following, post.Post, input.Feed) {
			candidates = append(candidates, post)
		}
	}
	var scores map[string]int64
	if ranked {
		scores, err = recommendationScores(ctx, db, viewer, candidates)
		if err != nil {
			return empty, err
		}
		sort.SliceStable(candidates, func(i, j int) bool { return scores[candidates[i].ID] > scores[candidates[j].ID] })
	}
	result := empty
	for _, post := range candidates {
		score := scores[post.ID]
		if ranked && input.BeforeScore != nil && !rankedBefore(score, post.Post, *input.BeforeScore, *input.BeforePostedAt, input.BeforeID) {
			continue
		}
		item := feedItemResponse{createPostResponse: newPostResponse(post.Post, post.Author)}
		if ranked {
			item.Score = &score
		}
		result.Items = append(result.Items, item)
		if len(result.Items) == 51 {
			break
		}
	}
	result.HasMore = len(result.Items) > 50
	if result.HasMore {
		result.Items = result.Items[:50]
	}
	return result, nil
}

func TestFeedPostgresMatchesOriginal(t *testing.T) {
	dsn := os.Getenv("ISUREAL_TEST_DB_DSN")
	if dsn == "" {
		t.Skip("set ISUREAL_TEST_DB_DSN for PostgreSQL integration tests")
	}
	db, err := sqlx.Connect("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	db.MustExec(`CREATE TEMP TABLE users (
 id text PRIMARY KEY, username text NOT NULL, secret_key_hash text NOT NULL,
 display_name text NOT NULL, bio text NOT NULL DEFAULT '', profile_image_id text,
 region text NOT NULL DEFAULT 'asia', account_type text NOT NULL DEFAULT 'regular', status text NOT NULL DEFAULT 'active')`)
	db.MustExec(`CREATE TEMP TABLE posting_windows (id text PRIMARY KEY, region text, local_date date, starts_at timestamptz, on_time_deadline timestamptz, ended_at timestamptz)`)
	db.MustExec(`CREATE TEMP TABLE posts (id text PRIMARY KEY,author_id text,posting_window_id text,image_id text,caption text,visibility text,
 location_kind text,latitude numeric,longitude numeric,locality text,posted_at timestamptz,deleted_at timestamptz)`)
	db.MustExec(`CREATE TEMP TABLE friendships (user_low_id text,user_high_id text)`)
	db.MustExec(`CREATE TEMP TABLE block_relations (blocker_id text,blocked_id text)`)
	db.MustExec(`CREATE TEMP TABLE follows (follower_id text,followee_id text)`)
	db.MustExec(`CREATE TEMP TABLE realmojis (post_id text,reactor_id text,replaced_at timestamptz,deleted_at timestamptz)`)
	db.MustExec(`CREATE TEMP TABLE comments (post_id text,author_id text,deleted_at timestamptz)`)
	db.MustExec(`INSERT INTO users(id,username,secret_key_hash,display_name,bio,profile_image_id,region,status)
 SELECT 'u'||lpad(i::text,3,'0'),'name'||i,repeat('00',32),'display'||i,'bio'||i,
 CASE WHEN i%6=0 THEN 'profile'||i END,CASE WHEN i%2=0 THEN 'europe' ELSE 'asia' END,
 CASE WHEN i%23=0 THEN 'deleted' ELSE 'active' END FROM generate_series(0,359) i`)
	db.MustExec(`INSERT INTO users(id,username,secret_key_hash,display_name) VALUES('viewer','viewer',repeat('00',32),'viewer')`)
	db.MustExec(`INSERT INTO posting_windows VALUES
 ('asia','asia','2026-10-31','2026-10-31','2026-10-31 00:10:00+00',NULL),
 ('europe','europe','2026-10-31','2026-10-31','2026-10-31 00:10:00+00',NULL),
 ('america','america','2026-11-01','2026-11-01','2026-11-01 00:10:00+00',NULL),
 ('old','asia','2026-10-30','2026-10-30','2026-10-30 00:10:00+00','2026-10-31'),
 ('moved','europe','2026-10-31','2026-10-31','2026-10-31 00:10:00+00','2026-10-31 01:00:00+00')`)
	// The viewer posted in a previous region/window with the current local date.
	db.MustExec(`INSERT INTO posts VALUES ('mine','viewer','moved','mine-image','','friends','none',NULL,NULL,NULL,'2026-10-31',NULL)`)
	db.MustExec(`INSERT INTO friendships SELECT LEAST('viewer','u'||lpad(i::text,3,'0')),GREATEST('viewer','u'||lpad(i::text,3,'0'))
 FROM generate_series(0,359,3) i`)
	db.MustExec(`INSERT INTO friendships SELECT LEAST('u003','u'||lpad(i::text,3,'0')),GREATEST('u003','u'||lpad(i::text,3,'0'))
 FROM generate_series(1,359,3) i`)
	db.MustExec(`INSERT INTO follows SELECT 'viewer','u'||lpad(i::text,3,'0') FROM generate_series(0,359,2) i`)
	db.MustExec(`INSERT INTO block_relations SELECT CASE WHEN i%2=0 THEN 'viewer' ELSE 'u'||lpad(i::text,3,'0') END,
 CASE WHEN i%2=0 THEN 'u'||lpad(i::text,3,'0') ELSE 'viewer' END FROM generate_series(0,359,11) i`)
	db.MustExec(`INSERT INTO posts
 SELECT 'p'||lpad(i::text,4,'0'),'u'||lpad((i%360)::text,3,'0'),
 CASE WHEN i%13=0 THEN 'old' WHEN i%3=0 THEN 'asia' WHEN i%3=1 THEN 'europe' ELSE 'america' END,
 'image'||i,'caption'||i,CASE i%4 WHEN 0 THEN 'public' WHEN 1 THEN 'friends' WHEN 2 THEN 'friends_of_friends' ELSE 'unknown' END,
 'none',NULL,NULL,NULL,'2026-10-31'::timestamptz+(i/3)*interval '1 second',
 CASE WHEN i%17=0 THEN '2026-10-31'::timestamptz END FROM generate_series(0,719) i`)
	// Many newest rows are invisible. LIMIT before authorization would hide older matches.
	db.MustExec(`INSERT INTO posts SELECT 'hidden'||i,'u000','asia','hidden-image'||i,'','public','none',NULL,NULL,NULL,
 '2026-10-31 10:00:00+00'::timestamptz+i*interval '1 second',NULL FROM generate_series(1,100) i`)
	db.MustExec(`INSERT INTO comments SELECT 'p'||lpad(i::text,4,'0'),'u003',NULL FROM generate_series(0,719,5) i`)
	db.MustExec(`INSERT INTO realmojis SELECT 'p'||lpad(i::text,4,'0'),'u003',NULL,NULL FROM generate_series(0,719,7) i`)
	e := echo.New()
	e.HTTPErrorHandler = APIErrorHandler
	e.GET("/api/feeds/:feed", func(c *echo.Context) error {
		c.Set("session", &Session{UserID: "viewer"})
		return getFeedHandler(&HandlerContext{DB: db})(c)
	})
	check := func(t *testing.T) {
		t.Helper()
		for _, id := range []string{"viewer", "u000", "u001", "u002", "u023", "missing"} {
			want, wantErr := referenceViewerContext(context.Background(), db, id)
			got, gotErr := loadViewerContext(context.Background(), db, id)
			if (wantErr == nil) != (gotErr == nil) || (wantErr != nil && !errors.Is(gotErr, wantErr)) {
				t.Fatalf("viewer %s error mismatch: got %v want %v", id, gotErr, wantErr)
			}
			if wantErr == nil && !reflect.DeepEqual(got, want) {
				t.Fatalf("viewer %s mismatch: got %#v want %#v", id, got, want)
			}
		}
		for _, feed := range []string{"friends", "public", "following", "friends-of-friends"} {
			input := getFeedRequest{Feed: feed}
			for page := 0; ; page++ {
				if page > 30 {
					t.Fatal("pagination did not finish")
				}
				want, err := referenceFeedPage(context.Background(), db, "viewer", input)
				if err != nil {
					t.Fatal(err)
				}
				q := url.Values{}
				if input.BeforePostedAt != nil {
					q.Set("beforePostedAt", input.BeforePostedAt.Format(time.RFC3339Nano))
					q.Set("beforeId", input.BeforeID)
				}
				if input.BeforeScore != nil {
					q.Set("beforeScore", fmt.Sprint(*input.BeforeScore))
				}
				r := httptest.NewRecorder()
				e.ServeHTTP(r, httptest.NewRequest("GET", "/api/feeds/"+feed+"?"+q.Encode(), nil))
				if r.Code != 200 {
					t.Fatalf("%s page %d: HTTP %d %s", feed, page, r.Code, r.Body.String())
				}
				// JSON roundtrip normalizes time locations and enforces array/null shape.
				wantJSON, err := json.Marshal(want)
				if err != nil {
					t.Fatal(err)
				}
				var gotValue, wantValue any
				if err := json.Unmarshal(r.Body.Bytes(), &gotValue); err != nil {
					t.Fatal(err)
				}
				if err := json.Unmarshal(wantJSON, &wantValue); err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(gotValue, wantValue) {
					t.Fatalf("%s page %d differs from original\ngot %s\nwant %s", feed, page, r.Body.String(), wantJSON)
				}
				if !want.HasMore {
					break
				}
				last := want.Items[len(want.Items)-1]
				input.BeforeID = last.ID
				before := last.PostedAt
				input.BeforePostedAt = &before
				input.BeforeScore = last.Score
			}
		}
	}
	t.Run("initial", check)
	cases := []struct{ name, sql string }{
		{"no-post", `UPDATE posts SET deleted_at=now() WHERE id='mine'`},
		{"restore-post", `UPDATE posts SET deleted_at=NULL WHERE id='mine'`},
		{"move-to-other-date", `UPDATE users SET region='america' WHERE id='viewer'`},
		{"move-to-same-date", `UPDATE users SET region='europe' WHERE id='viewer'`},
		{"retired-viewer", `UPDATE users SET status='deleted' WHERE id='viewer'`},
		{"restore-viewer", `UPDATE users SET status='active',region='asia' WHERE id='viewer'`},
		{"end-current-window", `UPDATE posting_windows SET ended_at=now() WHERE id='asia'`},
		{"restore-current-window", `UPDATE posting_windows SET ended_at=NULL WHERE id='asia'`},
		{"friend-removed", `DELETE FROM friendships WHERE user_low_id='u003' AND user_high_id='viewer'`},
		{"new-block", `INSERT INTO block_relations VALUES('u006','viewer')`},
		{"remove-block", `DELETE FROM block_relations WHERE blocker_id='u006' AND blocked_id='viewer'`},
		{"visibility-change", `UPDATE posts SET visibility='friends' WHERE author_id='u002'`},
		{"delete-author", `UPDATE users SET status='deleted' WHERE id='u012'`},
		{"unfollow", `DELETE FROM follows WHERE follower_id='viewer'`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) { db.MustExec(tc.sql); check(t) })
	}
	// Exact page boundaries and tied timestamps exercise id ordering and hasMore.
	for _, count := range []int{0, 50, 51, 100, 101} {
		t.Run(fmt.Sprintf("boundary-%d", count), func(t *testing.T) {
			db.MustExec(`DELETE FROM posts WHERE id<>'mine'`)
			db.MustExec(`UPDATE users SET status='active',region='asia' WHERE id='viewer'`)
			db.MustExec(`INSERT INTO posts SELECT 'boundary'||lpad(i::text,3,'0'),'u006','asia','boundary-image'||i,'','public','none',NULL,NULL,NULL,
    '2026-10-31'::timestamptz,NULL FROM generate_series(1,$1::int) i`, count)
			check(t)
		})
	}
}

func TestFeedRejectsInvalidPagination(t *testing.T) {
	e := echo.New()
	e.HTTPErrorHandler = APIErrorHandler
	e.GET("/api/feeds/:feed", getFeedHandler(&HandlerContext{}))
	for _, path := range []string{
		"/api/feeds/invalid",
		"/api/feeds/friends?beforeId=abc",
		"/api/feeds/public?beforePostedAt=2026-10-31T00:00:00Z",
		"/api/feeds/friends-of-friends?beforeScore=1",
		"/api/feeds/friends-of-friends?beforePostedAt=2026-10-31T00:00:00Z&beforeId=abc",
	} {
		t.Run(strings.ReplaceAll(path, "/", "_"), func(t *testing.T) {
			r := httptest.NewRecorder()
			e.ServeHTTP(r, httptest.NewRequest("GET", path, nil))
			if r.Code != 400 {
				t.Fatalf("HTTP %d: %s", r.Code, r.Body.String())
			}
		})
	}
}
