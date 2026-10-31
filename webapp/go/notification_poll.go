package isureal

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jmoiron/sqlx"
)

type notificationPollRow struct {
	Index           int            `db:"idx"`
	Region          string         `db:"region"`
	ID              sql.NullString `db:"id"`
	WindowID        sql.NullString `db:"posting_window_id"`
	WindowRegion    sql.NullString `db:"window_region"`
	CreatedAt       sql.NullTime   `db:"created_at"`
	Generation      int64          `db:"generation"`
	BatchCandidates []byte         `db:"batch_candidates"`
}

const notificationPollSQL = `SELECT req.idx, u.region, n.id, n.posting_window_id, n.window_region, n.created_at,
                       g.generation, b.candidates AS batch_candidates
                FROM (VALUES %s) AS req(idx, user_id, connected_at)
                JOIN users u ON u.id=req.user_id
                LEFT JOIN LATERAL (
                    SELECT n.id, n.posting_window_id, w.region AS window_region, n.created_at
                    FROM notifications n JOIN posting_windows w ON w.id = n.posting_window_id
                    WHERE n.user_id = u.id AND n.created_at > req.connected_at AND w.ended_at IS NULL
                    ORDER BY n.created_at, n.id LIMIT 1
                ) n ON true
                CROSS JOIN image_cache_generation g
                LEFT JOIN LATERAL (
                    SELECT COALESCE(jsonb_agg(jsonb_build_object(
                        'postingWindowId', b.posting_window_id,
                        'region', w.region, 'createdAt', b.created_at)
                        ORDER BY b.created_at, b.posting_window_id), '[]'::jsonb) AS candidates
                    FROM notification_batches b
                    JOIN posting_windows w ON w.id = b.posting_window_id
                    WHERE w.ended_at IS NULL AND b.created_at > req.connected_at
                ) b ON true
                WHERE u.status = 'active'`

type pollResult struct {
	row notificationPollRow
	err error
}
type notificationPoll struct {
	ctx         context.Context
	userID      string
	connectedAt time.Time
	result      chan pollResult
}
type notificationPollQueue struct {
	mu      sync.Mutex
	pending []notificationPoll
}

func (q *notificationPollQueue) read(ctx context.Context, db *sqlx.DB, userID string, connectedAt time.Time) (notificationPollRow, error) {
	req := notificationPoll{ctx, userID, connectedAt, make(chan pollResult, 1)}
	q.mu.Lock()
	q.pending = append(q.pending, req)
	if len(q.pending) == 1 {
		// Each timer owns its batch; a full batch can depart before the timer fires.
		batchID := req.result
		time.AfterFunc(2*time.Millisecond, func() {
			q.mu.Lock()
			if len(q.pending) == 0 || q.pending[0].result != batchID {
				q.mu.Unlock()
				return
			}
			batch := q.pending
			q.pending = nil
			q.mu.Unlock()
			runNotificationPoll(db, batch)
		})
	}
	if len(q.pending) == 64 {
		batch := q.pending
		q.pending = nil
		go runNotificationPoll(db, batch)
	}
	q.mu.Unlock()
	select {
	case <-ctx.Done():
		return notificationPollRow{}, ctx.Err()
	case r := <-req.result:
		return r.row, r.err
	}
}

func runNotificationPoll(db *sqlx.DB, batch []notificationPoll) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// A disconnected client must not cancel other subscribers' reads.
	var remaining atomic.Int32
	remaining.Store(int32(len(batch)))
	var values []string
	var args []any
	for i, r := range batch {
		stop := context.AfterFunc(r.ctx, func() {
			if remaining.Add(-1) == 0 {
				cancel()
			}
		})
		defer stop()
		values = append(values, fmt.Sprintf("(%d,$%d::text,$%d::timestamptz)", i, 2*i+1, 2*i+2))
		args = append(args, r.userID, r.connectedAt)
	}
	var rows []notificationPollRow
	err := db.SelectContext(ctx, &rows, fmt.Sprintf(notificationPollSQL, strings.Join(values, ",")), args...)
	results := make([]pollResult, len(batch))
	for i := range results {
		results[i].err = err
		if err == nil {
			results[i].err = sql.ErrNoRows
		}
	}
	if err == nil {
		for _, row := range rows {
			results[row.Index] = pollResult{row: row}
		}
	}
	for i, r := range batch {
		r.result <- results[i]
	}
}
