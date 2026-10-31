package isureal

import (
	"bytes"
	"database/sql"
	"errors"
	"io"
	"net/http"

	"example.com/isureal-company/isureal/internal/jpegvalidate"

	"github.com/google/uuid"
	"github.com/labstack/echo/v5"
)

func putProfileImageHandler(h *HandlerContext) echo.HandlerFunc {
	return func(c *echo.Context) error {
		if c.Request().Header.Get("Content-Type") != "image/jpeg" {
			return NewAPIError(http.StatusUnsupportedMediaType, "unsupported_media_type")
		}
		data, err := io.ReadAll(c.Request().Body)
		if err != nil {
			return err
		}
		img, err := jpegvalidate.Validate(bytes.NewReader(data))
		if err != nil {
			c.Set("abuse", "invalid_image")
			return NewAPIError(http.StatusBadRequest, "invalid_profile_image")
		}
		if img.Dx() > 512 || img.Dy() > 512 {
			return NewAPIError(http.StatusBadRequest, "invalid_profile_image")
		}

		ctx := c.Request().Context()
		userID := GetSession(c).UserID
		tx, err := h.DB.BeginTxx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		var oldID *string
		if err := tx.GetContext(ctx, &oldID, `SELECT profile_image_id FROM users WHERE id=$1 FOR NO KEY UPDATE`, userID); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return NewAPIError(http.StatusNotFound, "account_not_found")
			}
			return err
		}
		imageID := uuid.NewString()
		written, err := insertJPEG(ctx, tx, imageID, userID, data, nil)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE users SET profile_image_id=$2,updated_at=clock_timestamp() WHERE id=$1`, userID, imageID); err != nil {
			return err
		}
		if oldID != nil {
			if _, err := tx.ExecContext(ctx, `DELETE FROM image_objects WHERE id=$1 AND created_at > (SELECT created_at FROM initial_data_boundary)`, *oldID); err != nil {
				return err
			}
		}
		if err := tx.Commit(); err != nil {
			return err
		}
		invalidateImageMeta()
		h.ImageFiles.warmJPEG(written, imageID, data)
		return c.NoContent(http.StatusNoContent)
	}
}

func deleteProfileImageHandler(h *HandlerContext) echo.HandlerFunc {
	return func(c *echo.Context) error {
		ctx := c.Request().Context()
		userID := GetSession(c).UserID
		tx, err := h.DB.BeginTxx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		var imageID *string
		err = tx.GetContext(ctx, &imageID, `SELECT profile_image_id FROM users WHERE id=$1 FOR NO KEY UPDATE`, userID)
		if errors.Is(err, sql.ErrNoRows) {
			return NewAPIError(http.StatusNotFound, "account_not_found")
		}
		if err != nil {
			return err
		}
		if imageID == nil {
			return NewAPIError(http.StatusNotFound, "account_not_found")
		}
		if _, err := tx.ExecContext(ctx, `UPDATE users SET profile_image_id=NULL,updated_at=clock_timestamp() WHERE id=$1`, userID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM image_objects WHERE id=$1 AND created_at > (SELECT created_at FROM initial_data_boundary)`, *imageID); err != nil {
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
		invalidateImageMeta()
		return c.NoContent(http.StatusNoContent)
	}
}
