package isureal

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"
)

func TestNotificationPollSeparatesSubscriptions(t *testing.T) {
	db := isolatedWriteTestDB(t)
	db.MustExec(`CREATE TABLE users(id text primary key,region text,status text);
 CREATE TABLE posting_windows(id text,region text,ended_at timestamptz);
 CREATE TABLE notifications(id text,user_id text,posting_window_id text,created_at timestamptz);
 CREATE TABLE image_cache_generation(generation bigint);
 CREATE TABLE notification_batches(posting_window_id text,created_at timestamptz);
 INSERT INTO image_cache_generation VALUES(8);
 INSERT INTO users VALUES ('a','asia','active'),('b','europe','active'),('d','asia','deleted');
 INSERT INTO posting_windows VALUES('w','asia',NULL);
 INSERT INTO notifications VALUES('n','a','w','2026-10-31T05:00:00Z');
 INSERT INTO notification_batches VALUES('w','2026-10-31T05:00:00Z');`)
	tick := time.Date(2026, 10, 31, 5, 0, 0, 0, time.UTC)
	var batch []notificationPoll
	for i, user := range []string{"a", "a", "b", "d", "missing"} {
		start := tick.Add(-time.Second)
		if i == 1 {
			start = tick
		}
		batch = append(batch, notificationPoll{context.Background(), user, start, make(chan pollResult, 1)})
	}
	runNotificationPoll(db, batch)
	for i, request := range batch {
		result := <-request.result
		if i >= 3 {
			if !errors.Is(result.err, sql.ErrNoRows) {
				t.Fatalf("request %d: %v", i, result.err)
			}
			continue
		}
		if result.err != nil {
			t.Fatal(result.err)
		}
		if result.row.Index != i || result.row.Generation != 8 || (result.row.ID.String == "n") != (i == 0) || (result.row.Region == "europe") != (i == 2) || (string(result.row.BatchCandidates) == "[]") != (i == 1) {
			t.Fatalf("request %d: %+v", i, result.row)
		}
	}
}
