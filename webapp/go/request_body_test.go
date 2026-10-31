package isureal

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/labstack/echo/v5"
)

// An early error must not reset the connection while a large body is still being sent (#63).
func TestEarlyErrorDoesNotResetLargeUpload(t *testing.T) {
	e := echo.New()
	e.Use(ActionLogMiddleware("error", io.Discard))
	e.Use(DrainRequestBodyMiddleware)
	e.HTTPErrorHandler = APIErrorHandler
	e.PUT("/api/me/profile-image", func(c *echo.Context) error {
		return NewAPIError(http.StatusUnauthorized, "unauthorized")
	})
	server := httptest.NewServer(e)
	defer server.Close()

	conn, err := net.Dial("tcp", server.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	body := bytes.Repeat([]byte{0xff}, 8<<20)
	written := make(chan error, 1)
	go func() {
		_, err := fmt.Fprintf(conn, "PUT /api/me/profile-image HTTP/1.1\r\nHost: test\r\nContent-Type: image/jpeg\r\nContent-Length: %d\r\n\r\n", len(body))
		if err == nil {
			_, err = conn.Write(body)
		}
		written <- err
	}()
	res, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", res.StatusCode)
	}
	if err := <-written; err != nil {
		t.Fatalf("upload interrupted: %v", err)
	}
	if res.Close {
		t.Fatal("server closed the connection instead of keeping it alive")
	}
}
