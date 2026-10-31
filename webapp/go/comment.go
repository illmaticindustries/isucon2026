package isureal

import (
	"context"
	"time"

	"github.com/jmoiron/sqlx"
)

type Comment struct {
	ID        string     `db:"id"`
	PostID    string     `db:"post_id"`
	AuthorID  string     `db:"author_id"`
	Body      string     `db:"body"`
	CreatedAt time.Time  `db:"created_at"`
	DeletedAt *time.Time `db:"deleted_at"`
}

func getComment(ctx context.Context, db sqlx.QueryerContext, id string) (Comment, error) {
	var comment Comment
	err := sqlx.GetContext(ctx, db, &comment, `SELECT id,post_id,author_id,body,created_at,deleted_at FROM comments WHERE id=$1`, id)
	return comment, err
}

type CommentWithAuthor struct {
	Comment
	Author User `db:"author"`
}

var currentCommentSelect = `
  SELECT c.id, c.post_id, c.author_id, c.body, c.created_at, c.deleted_at, ` + userColumnsAs("u", "author") + `
  FROM comments c JOIN users u ON u.id = c.author_id AND u.status = 'active'
  WHERE c.deleted_at IS NULL `

func selectCommentsWithAuthor(ctx context.Context, db sqlx.QueryerContext, query string, args ...any) ([]CommentWithAuthor, error) {
	var comments []CommentWithAuthor
	if err := sqlx.SelectContext(ctx, db, &comments, query, args...); err != nil {
		return nil, err
	}
	for i := range comments {
		if err := comments[i].Author.setImageKey(); err != nil {
			return nil, err
		}
	}
	return comments, nil
}

func canComment(ctx context.Context, db sqlx.QueryerContext, viewerID, postID string) (bool, error) {
	return postAccessAllowed(ctx, db, viewerID, postID, true)
}
