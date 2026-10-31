package isureal

import (
	"context"
	"database/sql/driver"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
	"github.com/labstack/echo/v5"
)

// notificationRecipientQueries counts recipient snapshot loads across connections.
var notificationRecipientQueries atomic.Int64

func (c *notificationConn) batchRows(now time.Time, q string, args []driver.NamedValue) (driver.Rows, error) {
	if strings.Contains(q, "SELECT recipients") {
		notificationRecipientQueries.Add(1)
		if c.scenario == "batch-slow" {
			time.Sleep(200 * time.Millisecond)
		}
		payload := `{"u1":"n-batch"}`
		if c.scenario == "batch-late-recipient" || (c.scenario == "batch-second-window" && args[0].Value == "w-first") {
			payload = `{"other-user":"n-other"}`
		}
		if c.scenario == "batch-corrupt" {
			payload = `bad json`
		}
		if c.scenario == "batch-new-epoch" {
			payload = `{"u1":"n-new"}`
		}
		return &notificationRows{[]string{"recipients"}, [][]driver.Value{{[]byte(payload)}}}, nil
	}
	rows := &notificationRows{columns: []string{"region", "id", "posting_window_id", "window_region", "created_at", "generation", "batch_candidates"}}
	// After one empty poll, close the stream as an inactive user (no timeout).
	if c.scenario == "batch-inactive" || c.calls > 3 {
		return rows, nil
	}
	region := "asia"
	if c.scenario == "batch-moved" {
		region = "europe"
	}
	var oldID, oldWindow, oldRegion, oldAt driver.Value
	if c.scenario == "batch-old-first" {
		oldID = "n-old"
		oldWindow = "w-old"
		oldRegion = "asia"
		oldAt = now.Add(500 * time.Millisecond)
	}
	if c.scenario == "batch-new-first" {
		oldID = "n-old"
		oldWindow = "w-old"
		oldRegion = "asia"
		oldAt = now.Add(2 * time.Second)
	}
	batchRegion := "asia"
	if c.scenario == "batch-other-region" {
		batchRegion = "europe"
	}
	candidates := []getNotificationsResponse{{PostingWindowID: "w-batch", Region: batchRegion, CreatedAt: now.Add(time.Second)}}
	if c.scenario == "batch-second-window" {
		candidates = append([]getNotificationsResponse{{PostingWindowID: "w-first", Region: "europe", CreatedAt: now.Add(500 * time.Millisecond)}}, candidates...)
	}
	if c.scenario == "batch-ended-window" || c.scenario == "batch-before-connect" {
		candidates = nil
	}
	encoded, _ := json.Marshal(candidates)
	rows.values = [][]driver.Value{{region, oldID, oldWindow, oldRegion, oldAt, int64(1), encoded}}

	return rows, nil
}

func TestPersistedNotificationRecipientIdentity(t *testing.T) {
	recipients := makeNotificationRecipients([]string{"first", "second"})
	for _, id := range recipients {
		v, err := uuid.Parse(id)
		if err != nil || v.Version() != 4 {
			t.Fatal("notification must keep UUIDv4 format")
		}
	}
	if recipients["first"] == recipients["second"] {
		t.Fatal("duplicate notification IDs")
	}
	payload, err := json.Marshal(recipients)
	if err != nil {
		t.Fatal(err)
	}
	var restarted map[string]string
	if err = json.Unmarshal(payload, &restarted); err != nil {
		t.Fatal(err)
	}
	if restarted["first"] != recipients["first"] || restarted["second"] != recipients["second"] {
		t.Fatal("identity changed after reload")
	}
	if restarted["registered-later"] != "" {
		t.Fatal("new user became eligible for an old window")
	}
}
func TestNotificationBatchLifecycle(t *testing.T) {
	expected := map[string]string{
		"batch-active": "n-batch", "batch-second-window": "n-batch", "batch-other-region": "n-batch", "batch-inactive": "", "batch-moved": "n-batch",
		"batch-late-recipient": "", "batch-ended-window": "", "batch-before-connect": "",
		"batch-old-first": "n-old", "batch-new-first": "n-batch", "batch-corrupt": "",
	}
	for scenario, want := range expected {
		t.Run(scenario, func(t *testing.T) {
			notificationBatches.clear()
			db, err := sqlx.Open("notification-test", scenario)
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			req := httptest.NewRequest("GET", "/api/notifications", nil).WithContext(ctx)
			rec := httptest.NewRecorder()
			c := echo.New().NewContext(req, rec)
			c.Set("session", &Session{UserID: "u1", Region: "asia"})
			if err = getNotificationsHandler(&HandlerContext{DB: db})(c); err != nil {
				t.Fatal(err)
			}
			if ctx.Err() != nil {
				t.Fatal("stream did not close")
			}
			body := strings.TrimSpace(rec.Body.String())
			if want == "" {
				if body != "" {
					t.Fatalf("unexpected notification %s", body)
				}
				return
			}
			var n getNotificationsResponse
			if err = json.Unmarshal([]byte(strings.TrimPrefix(body, "data: ")), &n); err != nil {
				t.Fatal(err)
			}
			if n.ID != want {
				t.Fatalf("got %s want %s", n.ID, want)
			}
		})
	}
	notificationBatches.clear()
}
func TestNotificationBatchCacheEpochAndReload(t *testing.T) {
	db, err := sqlx.Open("notification-test", "batch-active")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	c := notificationBatchCache{batches: make(map[string]map[string]string)}
	for range 2 {
		id, err := c.recipient(context.Background(), db, 1, "w", "u1")
		if err != nil || id != "n-batch" {
			t.Fatalf("%q %v", id, err)
		}
	}
	c.clear() // Simulate a cold process; identity comes from the persisted payload.
	if id, err := c.recipient(context.Background(), db, 1, "w", "u1"); err != nil || id != "n-batch" {
		t.Fatal("reload changed identity")
	}
	newer, err := sqlx.Open("notification-test", "batch-new-epoch")
	if err != nil {
		t.Fatal(err)
	}
	defer newer.Close()
	if id, err := c.recipient(context.Background(), newer, 2, "w", "u1"); err != nil || id != "n-new" {
		t.Fatal("reset retained old recipient snapshot")
	}
	if id, err := c.recipient(context.Background(), db, 1, "w", "u1"); err != nil || id != "" {
		t.Fatal("stale snapshot accessed new epoch")
	}
	for i := range 25 {
		if _, err := c.recipient(context.Background(), newer, 2, fmt.Sprint(i), "u1"); err != nil {
			t.Fatal(err)
		}
	}
	if len(c.batches) > 12 {
		t.Fatal("unbounded batch retention")
	}
}
func BenchmarkNotificationSnapshot5000(b *testing.B) {
	ids := make([]string, 5000)
	for i := range ids {
		ids[i] = uuid.NewString()
	}
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		if _, err := json.Marshal(makeNotificationRecipients(ids)); err != nil {
			b.Fatal(err)
		}
	}
}

func TestNotificationBatchConcurrentReload(t *testing.T) {
	db, err := sqlx.Open("notification-test", "batch-active")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	c := notificationBatchCache{batches: make(map[string]map[string]string)}
	var wg sync.WaitGroup
	for range 32 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 20 {
				id, err := c.recipient(context.Background(), db, 1, "w", "u1")
				if err != nil || id != "n-batch" {
					t.Errorf("id=%q error=%v", id, err)
				}
			}
		}()
	}
	wg.Wait()
	if len(c.batches) != 1 {
		t.Fatal("same window produced multiple cache entries")
	}
}

func TestNotificationBatchLoadIsSharedAndUnlocked(t *testing.T) {
	db, err := sqlx.Open("notification-test", "batch-slow")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	c := notificationBatchCache{batches: map[string]map[string]string{"cached": {"u1": "n-cached"}}, generation: 1}
	notificationRecipientQueries.Store(0)
	var wg sync.WaitGroup
	for range 32 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if id, err := c.recipient(context.Background(), db, 1, "w-slow", "u1"); err != nil || id != "n-batch" {
				t.Errorf("id=%q error=%v", id, err)
			}
		}()
	}
	// While the slow load is in flight, a cached window must not wait for it.
	time.Sleep(50 * time.Millisecond)
	started := time.Now()
	if id, err := c.recipient(context.Background(), db, 1, "cached", "u1"); err != nil || id != "n-cached" {
		t.Fatalf("cached id=%q error=%v", id, err)
	}
	if elapsed := time.Since(started); elapsed > 50*time.Millisecond {
		t.Fatalf("cached window waited %s for another window's load", elapsed)
	}
	wg.Wait()
	if n := notificationRecipientQueries.Load(); n != 1 {
		t.Fatalf("%d recipient queries for one window", n)
	}
}

func TestNotificationBatchLeaderCancellationDoesNotFailWaiters(t *testing.T) {
	db, err := sqlx.Open("notification-test", "batch-slow")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	c := notificationBatchCache{batches: make(map[string]map[string]string)}
	leaderCtx, cancel := context.WithCancel(context.Background())
	leaderDone := make(chan struct{})
	go func() {
		defer close(leaderDone)
		_, _ = c.recipient(leaderCtx, db, 1, "w-slow", "u1")
	}()
	time.Sleep(50 * time.Millisecond)
	waiter := make(chan string, 1)
	go func() {
		id, err := c.recipient(context.Background(), db, 1, "w-slow", "u1")
		if err != nil {
			id = "error: " + err.Error()
		}
		waiter <- id
	}()
	time.Sleep(20 * time.Millisecond)
	cancel()
	if id := <-waiter; id != "n-batch" {
		t.Fatalf("waiter got %q after the leader was cancelled", id)
	}
	<-leaderDone
	if id, err := c.recipient(context.Background(), db, 1, "w-slow", "u1"); err != nil || id != "n-batch" {
		t.Fatal("loaded snapshot was not cached")
	}
}
