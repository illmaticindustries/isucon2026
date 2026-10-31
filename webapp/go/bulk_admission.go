package isureal

import "github.com/labstack/echo/v5"

// Share admission across regional listeners, just like the database pool.
// Wait before starting bulk queries/signing so ordinary operations retain
// database and CPU capacity. Cancellation never starts abandoned work.
var bulkAdmission = newBulkAdmission(1)

func newBulkAdmission(limit int) echo.MiddlewareFunc {
	slots := make(chan struct{}, limit)
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c *echo.Context) error {
			ctx := c.Request().Context()
			select {
			case slots <- struct{}{}:
				defer func() { <-slots }()
			case <-ctx.Done():
				return ctx.Err()
			}
			if err := ctx.Err(); err != nil {
				return err
			}
			return next(c)
		}
	}
}
