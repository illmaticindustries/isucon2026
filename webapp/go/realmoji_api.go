package isureal

import (
	"bytes"
	"database/sql"
	"errors"
	"io"
	"mime/multipart"
	"net/http"
	"time"

	"example.com/isureal-company/isureal/internal/jpegvalidate"

	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
	"github.com/labstack/echo/v5"
)

type replaceRealMojiRequest struct {
	PostID       string                `param:"postId" json:"-"`
	Image        *multipart.FileHeader `form:"image"`
	ReactionType string                `form:"reactionType"`
}

type replaceRealMojiResponse struct {
	ID           string                   `json:"id"`
	Reactor      relationshipUserResponse `json:"reactor"`
	ReactionType string                   `json:"reactionType"`
	ImageURL     string                   `json:"imageUrl"`
	CreatedAt    time.Time                `json:"createdAt"`
}

func newRealMojiResponse(reaction RealMoji, user User) replaceRealMojiResponse {
	return replaceRealMojiResponse{ID: reaction.ID, Reactor: newRelationshipUserResponse(user), ReactionType: reaction.ReactionType, ImageURL: signedImageURL(user, reaction.ImageID), CreatedAt: reaction.CreatedAt}
}

func replaceRealMojiHandler(h *HandlerContext) echo.HandlerFunc {
	return func(c *echo.Context) error {
		var input replaceRealMojiRequest
		if err := c.Bind(&input); err != nil {
			return err
		}
		if input.Image == nil {
			return NewAPIError(http.StatusBadRequest, "invalid_realmoji")
		}
		switch input.ReactionType {
		case "like", "smile", "surprise", "heart", "laugh", "instant":
		default:
			return NewAPIError(http.StatusBadRequest, "invalid_realmoji")
		}
		file, err := input.Image.Open()
		if err != nil {
			return err
		}
		defer file.Close()
		data, err := io.ReadAll(file)
		if err != nil {
			return err
		}
		img, err := jpegvalidate.Validate(bytes.NewReader(data))
		if err != nil {
			return NewAPIError(http.StatusBadRequest, "invalid_realmoji")
		}
		if img.Dx() > 512 || img.Dy() > 512 {
			return NewAPIError(http.StatusBadRequest, "invalid_realmoji")
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
		if actor == post.AuthorID {
			return NewAPIError(http.StatusForbidden, "interaction_forbidden")
		}
		tx, err := beginRelationshipTx(ctx, h.DB, actor, post.AuthorID)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		allowed, err := canViewPost(ctx, tx, actor, post.ID)
		if err != nil {
			return err
		}
		if !allowed {
			return NewAPIError(http.StatusForbidden, "interaction_forbidden")
		}
		if _, err := tx.ExecContext(ctx, `UPDATE realmojis SET replaced_at=clock_timestamp() WHERE post_id=$1 AND reactor_id=$2 AND replaced_at IS NULL AND deleted_at IS NULL`, post.ID, actor); err != nil {
			return err
		}
		imageID := uuid.NewString()
		written, err := insertJPEG(ctx, tx, imageID, actor, data, nil)
		if err != nil {
			return err
		}
		id := uuid.NewString()
		if _, err := tx.ExecContext(ctx, `INSERT INTO realmojis(id,post_id,reactor_id,image_id,reaction_type,created_at) VALUES($1,$2,$3,$4,$5,clock_timestamp())`, id, post.ID, actor, imageID, input.ReactionType); err != nil {
			return err
		}
		reaction, err := getRealMoji(ctx, tx, id)
		if err != nil {
			return err
		}
		user, err := getUser(ctx, tx, actor)
		if err != nil {
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
		h.ImageFiles.warmJPEG(written, imageID, data)
		return c.JSON(http.StatusCreated, newRealMojiResponse(reaction, user))
	}
}

type getRealMojisRequest struct {
	PostID          string     `param:"postId"`
	BeforeCreatedAt *time.Time `query:"beforeCreatedAt"`
	BeforeID        string     `query:"beforeId"`
}

type getRealMojisResponse struct {
	Items   []replaceRealMojiResponse `json:"items"`
	HasMore bool                      `json:"hasMore"`
}

func getRealMojisHandler(h *HandlerContext) echo.HandlerFunc {
	return func(c *echo.Context) error {
		var input getRealMojisRequest
		if err := c.Bind(&input); err != nil {
			return err
		}
		if (input.BeforeCreatedAt == nil) != (input.BeforeID == "") {
			return NewAPIError(http.StatusBadRequest, "invalid_pagination")
		}
		ctx := c.Request().Context()
		viewer := GetSession(c).UserID
		// View permission and the page are independent, so an inter-region
		// database link must not pay two round trips. The permission is evaluated
		// once in a CTE and the page joined to it; when it is false the join
		// yields one denial-only row instead of a separate SELECT EXISTS.
		var rows []struct {
			Allowed bool `db:"allowed"`
			RealMoji
			Reactor User `db:"reactor"`
		}
		if err := sqlx.SelectContext(ctx, h.DB, &rows, `
  WITH perm AS (
    SELECT EXISTS(SELECT 1`+permittedPostFrom+`) AS allowed
  )
  SELECT perm.allowed,
    COALESCE(r.id, '') AS id,
    COALESCE(r.post_id, '') AS post_id,
    COALESCE(r.reactor_id, '') AS reactor_id,
    COALESCE(r.image_id, '') AS image_id,
    COALESCE(r.reaction_type, '') AS reaction_type,
    COALESCE(r.created_at, 'epoch'::timestamptz) AS created_at,
    r.replaced_at,
    r.deleted_at,
    COALESCE(u.id, '') AS "reactor.id",
    COALESCE(u.username, '') AS "reactor.username",
    COALESCE(u.secret_key_hash, '') AS "reactor.secret_key_hash",
    COALESCE(u.display_name, '') AS "reactor.display_name",
    COALESCE(u.bio, '') AS "reactor.bio",
    u.profile_image_id AS "reactor.profile_image_id",
    COALESCE(u.region, '') AS "reactor.region",
    COALESCE(u.account_type, '') AS "reactor.account_type",
    COALESCE(u.status, '') AS "reactor.status"
  FROM perm
  LEFT JOIN realmojis r ON perm.allowed AND r.replaced_at IS NULL AND r.deleted_at IS NULL AND r.post_id=$1
    AND ($4::timestamptz IS NULL OR (r.created_at, r.id) < ($4, $5))
  LEFT JOIN users u ON u.id = r.reactor_id AND u.status = 'active'
  WHERE r.id IS NULL OR u.id IS NOT NULL
  ORDER BY r.created_at DESC, r.id DESC
  LIMIT 51
  `, input.PostID, viewer, false, input.BeforeCreatedAt, input.BeforeID); err != nil {
			return err
		}
		if len(rows) == 0 || !rows[0].Allowed {
			return NewAPIError(http.StatusForbidden, "interaction_forbidden")
		}
		reactions := make([]RealMojiWithReactor, 0, len(rows))
		for _, row := range rows {
			if row.RealMoji.ID == "" {
				continue
			}
			if err := row.Reactor.setImageKey(); err != nil {
				return err
			}
			reactions = append(reactions, RealMojiWithReactor{RealMoji: row.RealMoji, Reactor: row.Reactor})
		}
		hasMore := len(reactions) > 50
		if hasMore {
			reactions = reactions[:50]
		}
		items := make([]replaceRealMojiResponse, 0, len(reactions))
		for _, reaction := range reactions {
			items = append(items, newRealMojiResponse(reaction.RealMoji, reaction.Reactor))
		}
		return c.JSON(http.StatusOK, getRealMojisResponse{Items: items, HasMore: hasMore})
	}
}

type deleteRealMojiRequest struct {
	RealMojiID string `param:"realMojiId" json:"-"`
}

func deleteRealMojiHandler(h *HandlerContext) echo.HandlerFunc {
	return func(c *echo.Context) error {
		var input deleteRealMojiRequest
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
			ReactorID string `db:"reactor_id"`
			AuthorID  string `db:"author_id"`
		}
		err = tx.GetContext(ctx, &record, `SELECT r.reactor_id,p.author_id FROM realmojis r JOIN posts p ON p.id=r.post_id WHERE r.id=$1 AND r.deleted_at IS NULL FOR UPDATE OF r`, input.RealMojiID)
		if errors.Is(err, sql.ErrNoRows) {
			return NewAPIError(http.StatusNotFound, "interaction_not_found")
		}
		if err != nil {
			return err
		}
		if actor != record.ReactorID && actor != record.AuthorID {
			return NewAPIError(http.StatusForbidden, "interaction_forbidden")
		}
		if _, err := tx.ExecContext(ctx, `UPDATE realmojis SET deleted_at=clock_timestamp() WHERE id=$1`, input.RealMojiID); err != nil {
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
		return c.NoContent(http.StatusNoContent)
	}
}
