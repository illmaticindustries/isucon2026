package isureal

import (
	"io"

	"github.com/labstack/echo/v5"
)

// Matches nginx client_max_body_size.
const maxDrainedRequestBody = 32 << 20

// DrainRequestBodyMiddleware reads any request body left unread by early error returns
// (401, 403, 415, ...). Otherwise net/http closes the connection while nginx is still
// sending the body, and nginx replaces the response with 502.
func DrainRequestBodyMiddleware(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c *echo.Context) error {
		err := next(c)
		_, _ = io.Copy(io.Discard, io.LimitReader(c.Request().Body, maxDrainedRequestBody))
		return err
	}
}
