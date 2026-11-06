package isureal

import (
	"database/sql"
	"errors"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
	"github.com/labstack/echo/v5"
)

type createCommentRequest struct {
	PostID string `param:"postId" json:"-"`
	Body   string `json:"body"`
}

type createCommentResponse struct {
	ID        string                   `json:"id"`
	Author    relationshipUserResponse `json:"author"`
	Body      string                   `json:"body"`
	CreatedAt time.Time                `json:"createdAt"`
}

func newCommentResponse(comment Comment, user User) createCommentResponse {
	return createCommentResponse{ID: comment.ID, Author: newRelationshipUserResponse(user), Body: comment.Body, CreatedAt: comment.CreatedAt}
}

func createCommentHandler(h *HandlerContext) echo.HandlerFunc {
	return func(c *echo.Context) error {
		var input createCommentRequest
		if err := c.Bind(&input); err != nil {
			return err
		}
		if !validUserText(input.Body, 1, 1000) {
			return NewAPIError(http.StatusBadRequest, "invalid_interaction")
		}
		ctx := c.Request().Context()
		actor := GetSession(c).UserID
		post, err := getPost(ctx, h.DB, input.PostID)
		if errors.Is(err, sql.ErrNoRows) {
			return NewAPIError(http.StatusForbidden, "interaction_forbidden")
		}
		if err != nil {
			return err
		}
		tx, err := h.DB.BeginTxx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		_, err = lockRelationshipPair(ctx, tx, actor, post.AuthorID)
		if err != nil {
			return err
		}
		// Recheck access after acquiring the pair lock. Independent commenters
		// share the author's row lock, including on widely followed posts.
		var result CommentWithAuthor
		err = tx.GetContext(ctx, &result, `
  WITH inserted AS (
    INSERT INTO comments(id,post_id,author_id,body,created_at)
    SELECT $4,p.id,v.id,$5,clock_timestamp()`+permittedPostFrom+`
    RETURNING id,post_id,author_id,body,created_at,deleted_at
  )
  SELECT inserted.*, `+userColumnsAs("u", "author")+`
  FROM inserted JOIN users u ON u.id=inserted.author_id`,
			post.ID, actor, true, uuid.NewString(), input.Body)
		if errors.Is(err, sql.ErrNoRows) {
			return NewAPIError(http.StatusForbidden, "interaction_forbidden")
		}
		if err != nil {
			return err
		}
		if err := result.Author.setImageKey(); err != nil {
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
		return c.JSON(http.StatusCreated, newCommentResponse(result.Comment, result.Author))
	}
}

type getCommentsRequest struct {
	PostID         string     `param:"postId"`
	AfterCreatedAt *time.Time `query:"afterCreatedAt"`
	AfterID        string     `query:"afterId"`
}

type getCommentsResponse struct {
	Items   []createCommentResponse `json:"items"`
	HasMore bool                    `json:"hasMore"`
}

func getCommentsHandler(h *HandlerContext) echo.HandlerFunc {
	return func(c *echo.Context) error {
		var input getCommentsRequest
		if err := c.Bind(&input); err != nil {
			return err
		}
		if (input.AfterCreatedAt == nil) != (input.AfterID == "") {
			return NewAPIError(http.StatusBadRequest, "invalid_pagination")
		}
		ctx := c.Request().Context()
		viewer := GetSession(c).UserID
		// The comment permission and the page are independent of each other, so
		// an inter-region database link must not pay two round trips for them.
		// The permission is evaluated once in a CTE and the page is joined to it:
		// when it is false the join yields a single row that only carries the
		// denial, so no separate SELECT EXISTS is needed.
		var rows []struct {
			Allowed bool `db:"allowed"`
			Comment
			Author User `db:"author"`
		}
		if err := sqlx.SelectContext(ctx, h.DB, &rows, `
  WITH perm AS (
    SELECT EXISTS(SELECT 1`+permittedPostFrom+`) AS allowed
  )
  SELECT perm.allowed,
    COALESCE(c.id, '') AS id,
    COALESCE(c.post_id, '') AS post_id,
    COALESCE(c.author_id, '') AS author_id,
    COALESCE(c.body, '') AS body,
    COALESCE(c.created_at, 'epoch'::timestamptz) AS created_at,
    c.deleted_at,
    COALESCE(u.id, '') AS "author.id",
    COALESCE(u.username, '') AS "author.username",
    COALESCE(u.secret_key_hash, '') AS "author.secret_key_hash",
    COALESCE(u.display_name, '') AS "author.display_name",
    COALESCE(u.bio, '') AS "author.bio",
    u.profile_image_id AS "author.profile_image_id",
    COALESCE(u.region, '') AS "author.region",
    COALESCE(u.account_type, '') AS "author.account_type",
    COALESCE(u.status, '') AS "author.status"
  FROM perm
  LEFT JOIN comments c ON perm.allowed AND c.deleted_at IS NULL AND c.post_id=$1
    AND ($4::timestamptz IS NULL OR (c.created_at, c.id) > ($4, $5))
  LEFT JOIN users u ON u.id = c.author_id AND u.status = 'active'
  WHERE c.id IS NULL OR u.id IS NOT NULL
  ORDER BY c.created_at, c.id
  LIMIT 51
  `, input.PostID, viewer, true, input.AfterCreatedAt, input.AfterID); err != nil {
			return err
		}
		if len(rows) == 0 || !rows[0].Allowed {
			return NewAPIError(http.StatusForbidden, "interaction_forbidden")
		}
		comments := make([]CommentWithAuthor, 0, len(rows))
		for _, row := range rows {
			if row.Comment.ID == "" {
				continue
			}
			if err := row.Author.setImageKey(); err != nil {
				return err
			}
			comments = append(comments, CommentWithAuthor{Comment: row.Comment, Author: row.Author})
		}
		hasMore := len(comments) > 50
		if hasMore {
			comments = comments[:50]
		}
		items := make([]createCommentResponse, 0, len(comments))
		for _, comment := range comments {
			items = append(items, newCommentResponse(comment.Comment, comment.Author))
		}
		return c.JSON(http.StatusOK, getCommentsResponse{Items: items, HasMore: hasMore})
	}
}

type deleteCommentRequest struct {
	CommentID string `param:"commentId" json:"-"`
}

func deleteCommentHandler(h *HandlerContext) echo.HandlerFunc {
	return func(c *echo.Context) error {
		var input deleteCommentRequest
		if err := c.Bind(&input); err != nil {
			return err
		}
		ctx := c.Request().Context()
		actor := GetSession(c).UserID
		tx, err := h.DB.BeginTxx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		var lockedID string
		if err := tx.GetContext(ctx, &lockedID, `SELECT id FROM users WHERE id=$1 FOR NO KEY UPDATE`, actor); err != nil {
			return err
		}
		var record struct {
			AuthorID     string `db:"author_id"`
			PostAuthorID string `db:"post_author_id"`
		}
		err = tx.GetContext(ctx, &record, `SELECT c.author_id,p.author_id AS post_author_id FROM comments c JOIN posts p ON p.id=c.post_id WHERE c.id=$1 AND c.deleted_at IS NULL FOR UPDATE OF c`, input.CommentID)
		if errors.Is(err, sql.ErrNoRows) {
			return NewAPIError(http.StatusNotFound, "interaction_not_found")
		}
		if err != nil {
			return err
		}
		if actor != record.AuthorID && actor != record.PostAuthorID {
			return NewAPIError(http.StatusForbidden, "interaction_forbidden")
		}
		if _, err := tx.ExecContext(ctx, `UPDATE comments SET deleted_at=clock_timestamp() WHERE id=$1`, input.CommentID); err != nil {
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
		return c.NoContent(http.StatusNoContent)
	}
}
