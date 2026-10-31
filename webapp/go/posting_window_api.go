package isureal

import (
	"context"
	"net/http"
	"time"

	"github.com/labstack/echo/v5"
)

type currentPostingWindowResponse struct {
	ID             string    `json:"id"`
	Region         string    `json:"region"`
	LocalDate      string    `json:"localDate"`
	StartsAt       time.Time `json:"startsAt"`
	OnTimeDeadline time.Time `json:"onTimeDeadline"`
}

type getCurrentPostingWindowResponse struct {
	Window        *currentPostingWindowResponse `json:"window"`
	HasPosted     bool                          `json:"hasPosted"`
	CanPost       bool                          `json:"canPost"`
	DeletionCount int                           `json:"deletionCount"`
}

type postingWindowState struct {
	PostingWindow
	UserID  string `db:"user_id"`
	Active  int    `db:"active"`
	Deleted int    `db:"deleted"`
}

func (h *HandlerContext) readPostingWindows(ctx context.Context, ids []string) ([]*postingWindowState, error) {
	var rows []postingWindowState
	err := h.DB.SelectContext(ctx, &rows, `
   WITH open_windows AS MATERIALIZED (
     SELECT id,region,local_date,starts_at,on_time_deadline,ended_at FROM posting_windows WHERE ended_at IS NULL
   ), day_windows AS MATERIALIZED (
     SELECT id,local_date FROM posting_windows WHERE local_date IN (SELECT local_date FROM open_windows)
   ), slots AS MATERIALIZED (
     SELECT p.author_id,d.local_date,
            count(*) FILTER (WHERE p.deleted_at IS NULL) AS active,
            count(*) FILTER (WHERE p.deleted_at IS NOT NULL) AS deleted
     FROM posts p JOIN day_windows d ON p.posting_window_id=d.id
     WHERE p.posting_window_id = ANY(ARRAY(SELECT id FROM day_windows)) AND p.author_id=ANY($1::varchar[])
     GROUP BY p.author_id,d.local_date
   )
   SELECT u.id AS user_id,w.id,w.region,w.local_date,w.starts_at,w.on_time_deadline,w.ended_at,
          COALESCE(s.active,0) AS active,COALESCE(s.deleted,0) AS deleted
   FROM users u JOIN open_windows w ON w.region=u.region
   LEFT JOIN slots s ON s.author_id=u.id AND s.local_date=w.local_date
   WHERE u.id=ANY($1::varchar[])`, ids)
	if err != nil {
		return nil, err
	}
	byID := make(map[string]*postingWindowState, len(rows))
	for i := range rows {
		byID[rows[i].UserID] = &rows[i]
	}
	out := make([]*postingWindowState, len(ids))
	for i, id := range ids {
		out[i] = byID[id]
	}
	return out, nil
}

func getCurrentPostingWindowHandler(h *HandlerContext) echo.HandlerFunc {
	var reads readBatch[string, *postingWindowState]
	return func(c *echo.Context) error {
		state, err := reads.lookup(c.Request().Context(), GetSession(c).UserID, time.Millisecond, h.readPostingWindows)
		if err != nil {
			return err
		}
		if state == nil {
			return c.JSON(http.StatusOK, getCurrentPostingWindowResponse{})
		}
		return c.JSON(http.StatusOK, getCurrentPostingWindowResponse{
			Window: &currentPostingWindowResponse{
				ID: state.ID, Region: state.Region, LocalDate: state.LocalDate.Format(time.DateOnly),
				StartsAt: state.StartsAt, OnTimeDeadline: state.OnTimeDeadline,
			},
			HasPosted:     state.Active > 0,
			CanPost:       state.Active == 0 && state.Deleted < 2 && !time.Now().Before(state.StartsAt),
			DeletionCount: state.Deleted,
		})
	}
}
