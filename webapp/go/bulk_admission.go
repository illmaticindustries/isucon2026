package isureal

import (
	"sync/atomic"
	"time"

	"github.com/labstack/echo/v5"
)

// Share admission across regional listeners, just like the database pool.
// Wait before starting bulk queries/signing so ordinary operations retain
// database and CPU capacity. Cancellation never starts abandoned work.
var bulkLimiter = &bulkGate{slots: make(chan struct{}, 1)}
var bulkAdmission = bulkLimiter.middleware

type bulkGate struct {
	slots           chan struct{}
	waiting         atomic.Int64
	running         atomic.Int64
	started         atomic.Int64
	waitNanoseconds atomic.Int64
}

func (g *bulkGate) snapshot() map[string]int64 {
	return map[string]int64{"limit": int64(cap(g.slots)), "waiting": g.waiting.Load(),
		"running": g.running.Load(), "started": g.started.Load(), "wait_nanoseconds": g.waitNanoseconds.Load()}
}

func newBulkAdmission(limit int) echo.MiddlewareFunc {
	return (&bulkGate{slots: make(chan struct{}, limit)}).middleware
}

func (g *bulkGate) middleware(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c *echo.Context) error {
		ctx := c.Request().Context()
		start := time.Now()
		g.waiting.Add(1)
		select {
		case g.slots <- struct{}{}:
			defer func() { <-g.slots }()
		case <-ctx.Done():
			g.waiting.Add(-1)
			g.waitNanoseconds.Add(time.Since(start).Nanoseconds())
			return ctx.Err()
		}
		g.waiting.Add(-1)
		g.waitNanoseconds.Add(time.Since(start).Nanoseconds())
		if err := ctx.Err(); err != nil {
			return err
		}
		g.started.Add(1)
		g.running.Add(1)
		defer g.running.Add(-1)
		return next(c)
	}
}
