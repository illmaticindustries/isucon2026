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
	var polls notificationPollQueue
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
			// Return the active recipient even when no notification is ready so
			// retirement still terminates the stream promptly (the row vanishes).
			row, err := polls.read(ctx, h.DB, userID, connectedAt)
			if err != nil {
				if !errors.Is(err, sql.ErrNoRows) && ctx.Err() == nil {
					c.Logger().Error("failed to get notification recipient", "error", err)
				}
				return nil
			}
			// A region move must not drop the subscription. Keep this connection
			// and follow the user's current region: the window that opens after
			// the move is delivered here because createdAt > connectedAt, while a
			// client that reconnected after the window opened could no longer
			// receive it. Retirement still closes because the row disappears.
			session.Region = row.Region
			notification := getNotificationsResponse{
				ID: row.ID.String, PostingWindowID: row.WindowID.String,
				Region: row.WindowRegion.String, CreatedAt: row.CreatedAt.Time,
			}
			found := row.ID.Valid
			var batches []getNotificationsResponse
			if len(row.BatchCandidates) > 0 {
				if err := json.Unmarshal(row.BatchCandidates, &batches); err != nil {
					c.Logger().Error("failed to decode notification windows", "error", err)
					return nil
				}
			}
			// Check snapshots from all active regions, just like legacy rows.
			// A user may have moved away and back between two polling ticks.
			for _, batch := range batches {
				id, batchErr := notificationBatches.recipient(ctx, h.DB, row.Generation, batch.PostingWindowID, userID)
				if batchErr != nil {
					if ctx.Err() == nil {
						c.Logger().Error("failed to load notification batch", "error", batchErr)
					}
					return nil
				}
				if id != "" && (!found || batch.CreatedAt.Before(notification.CreatedAt) ||
					(batch.CreatedAt.Equal(notification.CreatedAt) && id < notification.ID)) {
					batch.ID = id
					notification = batch
					found = true
				}
			}

			if !found {
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
