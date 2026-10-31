package isureal

import (
	"database/sql"
	"encoding/json"
	"log"
	"net/http"
	_ "net/http/pprof"
	"os"
	"runtime"
	"sync/atomic"
	"time"
)

var diagnosticDB atomic.Pointer[sql.DB]
var diagnosticStarted = time.Now().UTC()

const blockSampleNanoseconds = 10_000_000 // sample roughly one per 10ms blocked
const mutexSampleFraction = 100           // sample one in 100 contention events

func dbStatsHandler(w http.ResponseWriter, r *http.Request) {
	db := diagnosticDB.Load()
	if db == nil {
		http.Error(w, "database not ready", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(struct {
		Time          time.Time        `json:"time"`
		Started       time.Time        `json:"started"`
		PID           int              `json:"pid"`
		BlockRate     int              `json:"block_rate_ns"`
		MutexFraction int              `json:"mutex_fraction"`
		DB            sql.DBStats      `json:"db"`
		Bulk          map[string]int64 `json:"bulk"`
	}{time.Now().UTC(), diagnosticStarted, os.Getpid(), blockSampleNanoseconds, mutexSampleFraction, db.Stats(), bulkLimiter.snapshot()})
}

// Profiling is reachable only from this host, never through the public router.
func init() {
	runtime.SetBlockProfileRate(blockSampleNanoseconds)
	runtime.SetMutexProfileFraction(mutexSampleFraction)
	http.HandleFunc("GET /debug/dbstats", dbStatsHandler)
	go func() {
		if err := http.ListenAndServe("127.0.0.1:6060", nil); err != nil {
			log.Printf("pprof listener: %v", err)
		}
	}()
}
