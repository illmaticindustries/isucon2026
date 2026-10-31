package isureal

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http/httptest"
	"testing"
	"time"
)

func TestDiagnosticStatsMeasuresPoolWait(t *testing.T) {
	old := diagnosticDB.Swap(nil)
	defer diagnosticDB.Store(old)
	r := httptest.NewRecorder()
	dbStatsHandler(r, httptest.NewRequest("GET", "/debug/dbstats", nil))
	if r.Code != 503 {
		t.Fatalf("not ready: %d", r.Code)
	}
	db, err := sql.Open("search-pagination-test", "")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	held, err := db.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if _, err := db.Conn(ctx); err == nil {
		t.Fatal("expected a pool timeout")
	}
	diagnosticDB.Store(db)
	r = httptest.NewRecorder()
	dbStatsHandler(r, httptest.NewRequest("GET", "/debug/dbstats", nil))
	var got struct {
		DB      sql.DBStats `json:"db"`
		Started time.Time   `json:"started"`
	}
	if err := json.Unmarshal(r.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if r.Code != 200 || got.DB.WaitCount != 1 || got.DB.WaitDuration <= 0 || got.DB.InUse != 1 || got.DB.MaxOpenConnections != 1 || !got.Started.Equal(diagnosticStarted) {
		t.Fatalf("unexpected pool metrics: %+v (HTTP %d)", got, r.Code)
	}
}
