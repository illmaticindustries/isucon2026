package isureal

import (
	"log"
	"net/http"
	_ "net/http/pprof"
)

// Profiling is reachable only from this host, never through the public router.
func init() {
	go func() {
		if err := http.ListenAndServe("127.0.0.1:6060", nil); err != nil {
			log.Printf("pprof listener: %v", err)
		}
	}()
}
