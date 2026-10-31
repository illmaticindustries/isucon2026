package isureal

import (
	"context"
	"database/sql"
	"errors"
	"reflect"
	"testing"

	"github.com/jmoiron/sqlx"
)

// Reuse the feed fixture after each block, friendship, region, window and
// account mutation. The oracle uses the original full relationship graph.
func checkPointPostAccess(t *testing.T, db *sqlx.DB) {
	t.Helper()
	ctx := context.Background()
	viewer, err := referenceViewerContext(ctx, db, "viewer")
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		t.Fatal(err)
	}
	active := err == nil
	for _, id := range []string{"mine", "p0001", "p0002", "p0004", "p0006", "p0010", "p0011", "p0012", "p0013", "p0017", "p0022", "p0023", "p0033", "missing"} {
		post, err := getPostWithAuthor(ctx, db, id)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			t.Fatal(err)
		}
		visible := active && err == nil && viewer.canView(post.Post)
		commentable := visible && post.WindowEndedAt == nil &&
			(viewer.ID == post.AuthorID && viewer.HasPostedToday || viewer.ID != post.AuthorID && (post.Visibility == "public" || viewer.Friends[post.AuthorID]))
		got, err := getVisiblePost(ctx, db, "viewer", id)
		if visible && (err != nil || !reflect.DeepEqual(post, got)) || !visible && !errors.Is(err, sql.ErrNoRows) {
			t.Fatalf("post %s visible=%t, lookup error=%v or payload differed", id, visible, err)
		}
		if ok, err := canViewPost(ctx, db, "viewer", id); err != nil || ok != visible {
			t.Fatalf("post %s view=%t want %t, err=%v", id, ok, visible, err)
		}
		if ok, err := canComment(ctx, db, "viewer", id); err != nil || ok != commentable {
			t.Fatalf("post %s comment=%t want %t, err=%v", id, ok, commentable, err)
		}
	}
}
