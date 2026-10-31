package isureal

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/labstack/echo/v5"
)

type getNotificationsResponse struct {
	ID              string    `json:"id" db:"id"`
	PostingWindowID string    `json:"postingWindowId" db:"posting_window_id"`
	Region          string    `json:"region" db:"region"`
	CreatedAt       time.Time `json:"createdAt" db:"created_at"`
}

func getNotificationsHandler(h *HandlerContext) echo.HandlerFunc {
	return func(c *echo.Context) error {
		ctx := c.Request().Context()
		session := GetSession(c)
		userID := session.UserID
		var connectedAt time.Time
		if err := h.DB.GetContext(ctx, &connectedAt, `SELECT clock_timestamp()`); err != nil {
			return err
		}
		response := c.Response()
		response.Header().Set("Content-Type", "text/event-stream")
		response.WriteHeader(http.StatusOK)
		controller := http.NewResponseController(response)
		if err := controller.Flush(); err != nil {
			return nil
		}
		ticker := time.NewTicker(500 * time.Millisecond)
		defer ticker.Stop()
		for {
			// Return the active recipient even when no notification is ready,
			// so retirement/region changes still terminate the stream promptly.
			var row struct {
				Region       string         `db:"region"`
				ID           sql.NullString `db:"id"`
				WindowID     sql.NullString `db:"posting_window_id"`
				WindowRegion sql.NullString `db:"window_region"`
				CreatedAt    sql.NullTime   `db:"created_at"`
			}
			err := h.DB.GetContext(ctx, &row, `
                SELECT u.region, n.id, n.posting_window_id, n.window_region, n.created_at
                FROM users u
                LEFT JOIN LATERAL (
                    SELECT n.id, n.posting_window_id, w.region AS window_region, n.created_at
                    FROM notifications n JOIN posting_windows w ON w.id = n.posting_window_id
                    WHERE n.user_id = u.id AND n.created_at > $2 AND w.ended_at IS NULL
                    ORDER BY n.created_at, n.id LIMIT 1
                ) n ON true
                WHERE u.id = $1 AND u.status = 'active'
            `, userID, connectedAt)
			if err != nil {
				if !errors.Is(err, sql.ErrNoRows) && ctx.Err() == nil {
					c.Logger().Error("failed to get notification recipient", "error", err)
				}
				return nil
			}
			if row.Region != session.Region {
				return nil
			}
			notification := getNotificationsResponse{
				ID: row.ID.String, PostingWindowID: row.WindowID.String,
				Region: row.WindowRegion.String, CreatedAt: row.CreatedAt.Time,
			}
			if !row.ID.Valid {
				err = sql.ErrNoRows
			}

			if err == nil {
				data, err := json.Marshal(notification)
				if err != nil {
					c.Logger().Error("failed to encode notification", "error", err)
					return nil
				}
				if _, err := fmt.Fprintf(response, "data: %s\n\n", data); err != nil {
					return nil
				}
				_ = controller.Flush()
				return nil
			}
			if !errors.Is(err, sql.ErrNoRows) {
				if ctx.Err() == nil {
					c.Logger().Error("failed to get notification", "error", err)
				}
				return nil
			}
			select {
			case <-ctx.Done():
				return nil
			case <-ticker.C:
			}
		}
	}
}
