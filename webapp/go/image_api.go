package isureal

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/labstack/echo/v5"
)

type getImageRequest struct {
	ImageID string `param:"imageId" json:"-"`
	Exp     string `query:"exp" json:"-"`
	Sig     string `query:"sig" json:"-"`
}

type imageMetadata struct {
	SecretKeyHash string    `db:"secret_key_hash"`
	Generation    int64     `db:"generation"`
	CreatedAt     time.Time `db:"created_at"`
	MediaType     string    `db:"media_type"`
	Source        string    `db:"source"`
}

// Revalidate after acquiring the durable lease. A deletion, reset or key
// change that raced with reading the bytes cannot seed the regional cache.
func (h *HandlerContext) rememberPostImage(c *echo.Context, metadata imageMetadata, id, file string, exp int64) {
	v, _ := c.Get("regional_read").(*regionalRead)
	if v == nil || metadata.Source != "post" || !h.regional.startImageFill(v.key) {
		return
	}
	// Echo reuses the request context. Pass only immutable values to this
	// bounded background check, and never hold the first image response for it.
	entry, ip := *v, c.RealIP()
	go func() {
		defer h.regional.finishImageFill(entry.key)
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		start := time.Now()
		var current imageMetadata
		if err := h.DB.GetContext(ctx, &current, `SELECT * FROM regional_image_grant($1,$2)`, id, ip); err != nil {
			return
		}
		if current.Generation != metadata.Generation || current.SecretKeyHash != metadata.SecretKeyHash ||
			current.MediaType != metadata.MediaType || !current.CreatedAt.Equal(metadata.CreatedAt) {
			return
		}
		entry.until = start.Add(imageReadLifetime)
		entry.file, entry.mediaType, entry.exp = file, metadata.MediaType, exp
		h.regional.store(entry)
		regionalReadCounts.imageFills.Add(1)
	}()
}

func getImageHandler(h *HandlerContext) echo.HandlerFunc {
	return func(c *echo.Context) error {
		c.Response().Header().Set("Cache-Control", "no-store")
		var input getImageRequest
		if err := c.Bind(&input); err != nil {
			return NewAPIError(http.StatusForbidden, "image_forbidden")
		}
		query, err := url.ParseQuery(c.Request().URL.RawQuery)
		if err != nil || len(query["exp"]) != 1 || len(query["sig"]) != 1 {
			return NewAPIError(http.StatusForbidden, "image_forbidden")
		}
		exp, err := strconv.ParseInt(input.Exp, 10, 64)
		if err != nil || strconv.FormatInt(exp, 10) != input.Exp || exp <= time.Now().Unix() {
			return NewAPIError(http.StatusForbidden, "image_forbidden")
		}
		sig, err := base64.RawURLEncoding.Strict().DecodeString(input.Sig)
		if err != nil || len(sig) != sha256.Size || base64.RawURLEncoding.EncodeToString(sig) != input.Sig {
			return NewAPIError(http.StatusForbidden, "image_forbidden")
		}

		ctx := c.Request().Context()
		var metadata imageMetadata
		err = h.DB.GetContext(ctx, &metadata, `
   SELECT u.secret_key_hash, g.generation, i.created_at, i.media_type,
     CASE WHEN u.profile_image_id = i.id THEN 'profile'
      WHEN EXISTS (SELECT 1 FROM posts p WHERE p.image_id = i.id AND p.author_id = u.id AND p.deleted_at IS NULL) THEN 'post'
      WHEN EXISTS (SELECT 1 FROM realmojis r
        JOIN posts p ON p.id = r.post_id AND p.deleted_at IS NULL
        JOIN users author ON author.id = p.author_id AND author.status = 'active'
        WHERE r.image_id = i.id AND r.reactor_id = u.id AND r.replaced_at IS NULL AND r.deleted_at IS NULL) THEN 'realmoji' ELSE '' END AS source
   FROM image_objects i JOIN users u ON u.id = i.owner_user_id
   CROSS JOIN image_cache_generation g
   WHERE i.id = $1 AND u.status = 'active'
  `, input.ImageID)
		if errors.Is(err, sql.ErrNoRows) {
			return NewAPIError(http.StatusForbidden, "image_forbidden")
		}
		if err != nil {
			return err
		}
		key, err := imageSigningKey(metadata.SecretKeyHash)
		if err != nil {
			return err
		}
		if !hmac.Equal(sig, imageSignature(key, input.ImageID, exp)) {
			c.Set("abuse", "invalid_signature")
			return NewAPIError(http.StatusForbidden, "image_forbidden")
		}

		// Signature failures must still be recorded even for an invalid image.
		if metadata.Source == "" {
			return NewAPIError(http.StatusForbidden, "image_forbidden")
		}
		if v, _ := c.Get("regional_read").(*regionalRead); v != nil && metadata.Source == "post" {
			regionalReadCounts.imageMisses.Add(1)
		}
		cacheKey := imageBodyKey{ID: input.ImageID, CreatedAt: metadata.CreatedAt, OwnerKey: metadata.SecretKeyHash}
		fileKey := ""
		if h.ImageFiles != nil && canAccelImage(c.Request()) {
			fileKey = fileImageKey(cacheKey.ID, cacheKey.CreatedAt, metadata.MediaType, metadata.Generation)
		}
		serveFile := func() error {
			if exp <= time.Now().Unix() {
				return NewAPIError(http.StatusForbidden, "image_forbidden")
			}
			h.rememberPostImage(c, metadata, input.ImageID, fileKey, exp)
			c.Response().Header().Set("Content-Type", metadata.MediaType)
			c.Response().Header().Set("X-Accel-Redirect", imageFilePrefix+fileKey)
			return c.NoContent(http.StatusOK)
		}
		if h.ImageFiles.exists(fileKey) {
			return serveFile()
		}
		if body, ok := imageBodies.get(metadata.Generation, cacheKey); ok {
			if fileKey != "" && h.ImageFiles.put(fileKey, metadata.Generation, body.Data) {
				return serveFile()
			}
			if exp <= time.Now().Unix() {
				return NewAPIError(http.StatusForbidden, "image_forbidden")
			}
			return c.Blob(http.StatusOK, body.MediaType, body.Data)
		}
		// On a miss recheck validity and generation in the same snapshot as the data.
		var object struct {
			Data      []byte `db:"data"`
			MediaType string `db:"media_type"`
		}
		err = h.DB.GetContext(ctx, &object, `
   SELECT i.data, i.media_type
   FROM image_objects i JOIN users u ON u.id = i.owner_user_id AND u.status = 'active'
   WHERE i.id = $1 AND i.created_at = $2
     AND (SELECT generation FROM image_cache_generation) = $3 AND (
     u.profile_image_id = i.id
     OR EXISTS (
       SELECT 1 FROM posts p
       WHERE p.image_id = i.id AND p.author_id = u.id AND p.deleted_at IS NULL
     )
     OR EXISTS (
       SELECT 1 FROM realmojis r
       JOIN posts p ON p.id = r.post_id AND p.deleted_at IS NULL
       JOIN users author ON author.id = p.author_id AND author.status = 'active'
       WHERE r.image_id = i.id AND r.reactor_id = u.id AND r.replaced_at IS NULL AND r.deleted_at IS NULL
     )
   )
  `, input.ImageID, metadata.CreatedAt, metadata.Generation)
		if errors.Is(err, sql.ErrNoRows) {
			return NewAPIError(http.StatusForbidden, "image_forbidden")
		}
		if err != nil {
			return err
		}
		if exp <= time.Now().Unix() {
			return NewAPIError(http.StatusForbidden, "image_forbidden")
		}
		imageBodies.put(metadata.Generation, cacheKey, cachedImageBody{Data: object.Data, MediaType: object.MediaType})
		if fileKey != "" && h.ImageFiles.put(fileKey, metadata.Generation, object.Data) {
			return serveFile()
		}
		return c.Blob(http.StatusOK, object.MediaType, object.Data)
	}
}
