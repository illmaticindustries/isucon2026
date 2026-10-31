package isureal

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
)

// Called inside the same transaction and users-table lock as window creation.
// Eligibility cannot drift between the snapshot and commit.
func insertNotificationBatch(ctx context.Context, tx *sqlx.Tx, region, windowID string, startsAt time.Time) error {
	var ids []string
	if err := tx.SelectContext(ctx, &ids, `SELECT id FROM users WHERE region=$1 AND status='active'`, region); err != nil {
		return err
	}
	recipients := makeNotificationRecipients(ids)
	payload, err := json.Marshal(recipients)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO notification_batches(posting_window_id,created_at,recipients) VALUES($1,$2,$3::jsonb)`, windowID, startsAt, string(payload))
	return err
}

type notificationBatchCache struct {
	mu         sync.Mutex
	generation int64
	batches    map[string]map[string]string
}

var notificationBatches = notificationBatchCache{batches: make(map[string]map[string]string)}

func (c *notificationBatchCache) clear() { c.mu.Lock(); defer c.mu.Unlock(); clear(c.batches) }

// Every caller first queries live user status/region, active window and epoch.
// This cache contains only immutable recipient snapshots, never authorization.
func (c *notificationBatchCache) recipient(ctx context.Context, db sqlx.QueryerContext, generation int64, windowID, userID string) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if generation < c.generation {
		return "", nil
	}
	if generation > c.generation {
		clear(c.batches)
		c.generation = generation
	}
	recipients, ok := c.batches[windowID]
	if !ok {
		var payload []byte
		err := sqlx.GetContext(ctx, db, &payload, `SELECT recipients FROM notification_batches WHERE posting_window_id=$1`, windowID)
		if errors.Is(err, sql.ErrNoRows) {
			return "", nil
		}
		if err != nil {
			return "", err
		}
		if err = json.Unmarshal(payload, &recipients); err != nil {
			return "", err
		}
		// At most a few current windows are hot; bound retained historical batches.
		if len(c.batches) >= 12 {
			clear(c.batches)
		}
		c.batches[windowID] = recipients
	}
	return recipients[userID], nil
}

func makeNotificationRecipients(ids []string) map[string]string {
	recipients := make(map[string]string, len(ids))
	for _, id := range ids {
		recipients[id] = uuid.NewString()
	}
	return recipients
}
