package isureal

import (
	"archive/zip"
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/labstack/echo/v5"
)

type exportPostResponse struct {
	createPostResponse
	ImagePath string `json:"imagePath"`
}

func exportPostsHandler(h *HandlerContext) echo.HandlerFunc {
	return func(c *echo.Context) error {
		ctx := c.Request().Context()
		userID := GetSession(c).UserID
		author, err := getUser(ctx, h.DB, userID)
		if err != nil {
			return err
		}
		// Image bytes are read separately: an export carries every post image of
		// the author (about 50MB for a long history), and reading them from the
		// shared database evicted its hot pages for all other queries.
		var posts []struct {
			Post
			MediaType       string       `db:"media_type"`
			Generation      int64        `db:"generation"`
			InitialBoundary sql.NullTime `db:"initial_boundary"`
			ImageData       []byte       `db:"-"`
		}
		if err := h.DB.SelectContext(ctx, &posts, `
   SELECT `+postColumns+`, i.media_type, (SELECT generation FROM image_cache_generation) AS generation,
     (SELECT created_at FROM initial_data_boundary) AS initial_boundary
   FROM posts p
   JOIN posting_windows w ON w.id = p.posting_window_id
   -- A post image is stored with created_at = posted_at, so this prunes to one partition instead of probing all.
   JOIN image_objects i ON i.id = p.image_id AND i.created_at = p.posted_at
   WHERE p.author_id = $1 AND p.deleted_at IS NULL
   ORDER BY p.posted_at, p.id
  `, userID); err != nil {
			return err
		}
		// Images are immutable, so this host's file cache (filled when an image
		// is created or served here, and kept across resets for initial-data
		// images) has the same bytes as the database.
		var missingIDs []string
		var missingAt []time.Time
		var missingKeys []string
		for i := range posts {
			post := &posts[i]
			key := imageFileKey(post.ImageID, post.PostedAt, post.MediaType, post.Generation, post.InitialBoundary)
			if body, ok := h.ImageFiles.read(key); ok {
				post.ImageData = body
				continue
			}
			missingIDs = append(missingIDs, post.ImageID)
			missingAt = append(missingAt, post.PostedAt)
			missingKeys = append(missingKeys, key)
		}
		if len(missingIDs) > 0 {
			var images []struct {
				ID   string `db:"id"`
				Data []byte `db:"data"`
			}
			// created_at = ANY prunes to the partitions holding these images.
			if err := h.DB.SelectContext(ctx, &images, `
   SELECT i.id, i.data FROM image_objects i
   WHERE i.id = ANY($1) AND i.created_at = ANY($2)
  `, missingIDs, missingAt); err != nil {
				return err
			}
			byID := make(map[string][]byte, len(images))
			for _, image := range images {
				byID[image.ID] = image.Data
			}
			for i := range posts {
				if posts[i].ImageData == nil {
					data, ok := byID[posts[i].ImageID]
					if !ok {
						return fmt.Errorf("export image %s disappeared", posts[i].ImageID)
					}
					posts[i].ImageData = data
				}
			}
			warm := make([]exportImageFile, 0, len(missingIDs))
			for i, id := range missingIDs {
				warm = append(warm, exportImageFile{key: missingKeys[i], data: byID[id]})
			}
			if len(posts) > 0 {
				h.ImageFiles.warmExport(posts[0].Generation, warm)
			}
		}

		var body bytes.Buffer
		writer := zip.NewWriter(&body)
		defer writer.Close()
		items := make([]exportPostResponse, 0, len(posts))
		for _, post := range posts {
			imagePath := fmt.Sprintf("images/%06d.jpg", len(items)+1)
			image, err := writer.CreateHeader(&zip.FileHeader{Name: imagePath, Method: zip.Store})
			if err != nil {
				return err
			}
			if _, err := image.Write(post.ImageData); err != nil {
				return err
			}
			items = append(items, exportPostResponse{createPostResponse: newPostResponse(post.Post, author), ImagePath: imagePath})
		}
		manifest, err := writer.Create("posts.json")
		if err != nil {
			return err
		}
		if err := json.NewEncoder(manifest).Encode(items); err != nil {
			return err
		}
		if err := writer.Close(); err != nil {
			return err
		}
		c.Response().Header().Set(echo.HeaderContentDisposition, `attachment; filename="isureal-posts.zip"`)
		c.Response().Header().Set(echo.HeaderCacheControl, "no-store")
		return c.Blob(http.StatusOK, "application/zip", body.Bytes())
	}
}
