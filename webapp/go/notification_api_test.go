package isureal

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"io"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jmoiron/sqlx"
	"github.com/labstack/echo/v5"
)

type notificationDriver struct{}
type notificationConn struct {
	scenario string
	calls    int
}
type notificationRows struct {
	columns []string
	values  [][]driver.Value
}

func (notificationDriver) Open(s string) (driver.Conn, error) {
	return &notificationConn{scenario: s}, nil
}
func (c *notificationConn) Prepare(string) (driver.Stmt, error) {
	return nil, fmt.Errorf("unsupported")
}
func (c *notificationConn) Close() error              { return nil }
func (c *notificationConn) Begin() (driver.Tx, error) { return nil, fmt.Errorf("unsupported") }
func (r *notificationRows) Columns() []string         { return r.columns }
func (r *notificationRows) Close() error              { return nil }
func (r *notificationRows) Next(v []driver.Value) error {
	if len(r.values) == 0 {
		return io.EOF
	}
	copy(v, r.values[0])
	r.values = r.values[1:]
	return nil
}
func (c *notificationConn) QueryContext(ctx context.Context, q string, args []driver.NamedValue) (driver.Rows, error) {
	c.calls++
	now := time.Date(2026, 10, 31, 0, 0, 0, 0, time.UTC)
	if strings.Contains(q, "clock_timestamp") {
		return &notificationRows{[]string{"clock_timestamp"}, [][]driver.Value{{now}}}, nil
	}
	r := &notificationRows{columns: []string{"region", "id", "posting_window_id", "window_region", "created_at"}}
	if c.scenario == "inactive" || (c.scenario == "empty-then-inactive" && c.calls > 2) {
		return r, nil
	}
	if c.scenario == "empty-then-inactive" {
		r.values = [][]driver.Value{{"asia", nil, nil, nil, nil}}
		return r, nil
	}
	region := "asia"
	if c.scenario == "moved" {
		region = "europe"
	}
	r.values = [][]driver.Value{{region, "n1", "w1", "asia", now.Add(time.Second)}}
	return r, nil
}
func init() { sql.Register("notification-test", notificationDriver{}) }
func TestNotificationRecipientLifecycle(t *testing.T) {
	for _, scenario := range []string{"active", "inactive", "moved", "empty-then-inactive"} {
		t.Run(scenario, func(t *testing.T) {
			db, err := sqlx.Open("notification-test", scenario)
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			req := httptest.NewRequest("GET", "/api/notifications", nil).WithContext(ctx)
			rec := httptest.NewRecorder()
			c := echo.New().NewContext(req, rec)
			c.Set("session", &Session{UserID: "u1", Region: "asia"})
			if err := getNotificationsHandler(&HandlerContext{DB: db})(c); err != nil {
				t.Fatal(err)
			}
			if ctx.Err() != nil {
				t.Fatal("stream failed to close when recipient became inactive")
			}
			sent := strings.Contains(rec.Body.String(), `"id":"n1"`)
			if sent != (scenario == "active") {
				t.Fatalf("unexpected event: %s", rec.Body.String())
			}
		})
	}
}
