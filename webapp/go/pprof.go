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

// profilingEnabled reports whether the block/mutex profilers are active. They
// instrument every blocking and contention event, which costs CPU and latency on
// the request path, so they are off unless ISUREAL_PROFILE asks for them (run
// make measure with it set when a profile is actually wanted).
func profilingEnabled() bool {
	switch os.Getenv("ISUREAL_PROFILE") {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

func dbStatsHandler(w http.ResponseWriter, r *http.Request) {
	db := diagnosticDB.Load()
	if db == nil {
		http.Error(w, "database not ready", http.StatusServiceUnavailable)
		return
	}
	blockRate, mutexFraction := 0, 0
	if profilingEnabled() {
		blockRate, mutexFraction = blockSampleNanoseconds, mutexSampleFraction
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(struct {
		Time          time.Time   `json:"time"`
		Started       time.Time   `json:"started"`
		PID           int         `json:"pid"`
		BlockRate     int         `json:"block_rate_ns"`
		MutexFraction int         `json:"mutex_fraction"`
		DB            sql.DBStats `json:"db"`
	}{time.Now().UTC(), diagnosticStarted, os.Getpid(), blockRate, mutexFraction, db.Stats()})
}

// Profiling is reachable only from this host, never through the public router.
func init() {
	if profilingEnabled() {
		runtime.SetBlockProfileRate(blockSampleNanoseconds)
		runtime.SetMutexProfileFraction(mutexSampleFraction)
	}
	http.HandleFunc("GET /debug/dbstats", dbStatsHandler)
	go func() {
		if err := http.ListenAndServe("127.0.0.1:6060", nil); err != nil {
			log.Printf("pprof listener: %v", err)
		}
	}()
}
