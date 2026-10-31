package isureal

import (
	"database/sql"
	"fmt"
	"os"
	"time"

	"github.com/labstack/echo/v5"
)

// SlowRequestMiddleware reports requests that took at least threshold, together
// with how much of that time was spent waiting for a database connection.
// Diagnostic only: remove once the source of the latency is known.
func SlowRequestMiddleware(db *sql.DB, threshold time.Duration) echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c *echo.Context) error {
			before := db.Stats()
			start := time.Now()
			err := next(c)
			elapsed := time.Since(start)
			if elapsed >= threshold {
				after := db.Stats()
				route := c.RouteInfo()
				fmt.Fprintf(os.Stdout,
					"slow_request route=%s method=%s duration_ms=%d db_wait_ms=%d db_wait_count=%d db_open=%d db_inuse=%d\n",
					route.Path, route.Method, elapsed.Milliseconds(),
					(after.WaitDuration - before.WaitDuration).Milliseconds(),
					after.WaitCount-before.WaitCount, after.OpenConnections, after.InUse)
			}
			return err
		}
	}
}
