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
	loading    map[notificationBatchKey]*notificationBatchLoad
}

type notificationBatchKey struct {
	generation int64
	windowID   string
}

// notificationBatchLoad lets concurrent pollers of a new window share one query.
type notificationBatchLoad struct {
	done       chan struct{}
	recipients map[string]string
	err        error
}

var notificationBatches = notificationBatchCache{batches: make(map[string]map[string]string)}

func (c *notificationBatchCache) clear() { c.mu.Lock(); defer c.mu.Unlock(); clear(c.batches) }

// Every caller first queries live user status/region, active window and epoch.
// This cache contains only immutable recipient snapshots, never authorization.
func (c *notificationBatchCache) recipient(ctx context.Context, db sqlx.QueryerContext, generation int64, windowID, userID string) (string, error) {
	c.mu.Lock()
	if generation < c.generation {
		c.mu.Unlock()
		return "", nil
	}
	if generation > c.generation {
		clear(c.batches)
		c.generation = generation
	}
	if recipients, ok := c.batches[windowID]; ok {
		c.mu.Unlock()
		return recipients[userID], nil
	}
	// Query without holding the lock: every SSE poller passes through here.
	key := notificationBatchKey{generation: generation, windowID: windowID}
	load, waiting := c.loading[key]
	if !waiting {
		if c.loading == nil {
			c.loading = make(map[notificationBatchKey]*notificationBatchLoad)
		}
		load = &notificationBatchLoad{done: make(chan struct{})}
		c.loading[key] = load
	}
	c.mu.Unlock()

	if waiting {
		select {
		case <-load.done:
		case <-ctx.Done():
			return "", ctx.Err()
		}
		return load.recipients[userID], load.err
	}

	// Waiters must not fail because this caller's request was cancelled.
	loadCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	load.recipients, load.err = loadNotificationRecipients(loadCtx, db, windowID)
	cancel()
	c.mu.Lock()
	delete(c.loading, key)
	if load.err == nil && load.recipients != nil && generation == c.generation {
		// At most a few current windows are hot; bound retained historical batches.
		if len(c.batches) >= 12 {
			clear(c.batches)
		}
		c.batches[windowID] = load.recipients
	}
	c.mu.Unlock()
	close(load.done)
	return load.recipients[userID], load.err
}

// loadNotificationRecipients returns nil without error for an unknown window.
func loadNotificationRecipients(ctx context.Context, db sqlx.QueryerContext, windowID string) (map[string]string, error) {
	var payload []byte
	err := sqlx.GetContext(ctx, db, &payload, `SELECT recipients FROM notification_batches WHERE posting_window_id=$1`, windowID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var recipients map[string]string
	if err := json.Unmarshal(payload, &recipients); err != nil {
		return nil, err
	}
	return recipients, nil
}

func makeNotificationRecipients(ids []string) map[string]string {
	recipients := make(map[string]string, len(ids))
	for _, id := range ids {
		recipients[id] = uuid.NewString()
	}
	return recipients
}
