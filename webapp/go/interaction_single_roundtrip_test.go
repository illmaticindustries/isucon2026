package isureal

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"fmt"
	"io"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jmoiron/sqlx"
	"github.com/labstack/echo/v5"
)

// singleRoundTripDriver returns canned rows so the collapsed single-statement
// handlers can be exercised without PostgreSQL. The query text is not parsed;
// mapping and authorization branches are what these tests pin down.
type singleRoundTripDriver struct{}

type singleRoundTripConn struct{}

type singleRoundTripRows struct {
	columns []string
	values  [][]driver.Value
}

var singleRoundTripCanned struct {
	columns []string
	values  [][]driver.Value
}

func (singleRoundTripDriver) Open(string) (driver.Conn, error) { return singleRoundTripConn{}, nil }

func (singleRoundTripConn) Prepare(string) (driver.Stmt, error) {
	return nil, fmt.Errorf("unsupported")
}
func (singleRoundTripConn) Close() error              { return nil }
func (singleRoundTripConn) Begin() (driver.Tx, error) { return nil, fmt.Errorf("unsupported") }

func (singleRoundTripConn) QueryContext(context.Context, string, []driver.NamedValue) (driver.Rows, error) {
	return &singleRoundTripRows{columns: singleRoundTripCanned.columns, values: singleRoundTripCanned.values}, nil
}

func (rows *singleRoundTripRows) Columns() []string { return rows.columns }
func (rows *singleRoundTripRows) Close() error      { return nil }
func (rows *singleRoundTripRows) Next(dest []driver.Value) error {
	if len(rows.values) == 0 {
		return io.EOF
	}
	copy(dest, rows.values[0])
	rows.values = rows.values[1:]
	return nil
}

func init() { sql.Register("single-roundtrip-test", singleRoundTripDriver{}) }

func singleRoundTripDB(t *testing.T) *sqlx.DB {
	t.Helper()
	db, err := sqlx.Open("single-roundtrip-test", "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func TestGetUserHandlerCollapsedQuery(t *testing.T) {
	db := singleRoundTripDB(t)
	e := echo.New()
	e.HTTPErrorHandler = APIErrorHandler
	e.GET("/api/users/:userId", func(c *echo.Context) error {
		c.Set("session", &Session{UserID: "viewer"})
		return getUserHandler(&HandlerContext{DB: db})(c)
	})

	base := []driver.Value{"u2", "u2", strings.Repeat("00", 32), "Display", "secret bio", nil, "asia", "regular", "active"}

	// Blocked target: one row, is_blocked=true -> 404 with the relationship error.
	singleRoundTripCanned.columns = append(append([]string{}, userColumns...), "is_blocked", "is_friend")
	singleRoundTripCanned.values = [][]driver.Value{{base[0], base[1], base[2], base[3], base[4], base[5], base[6], base[7], base[8], true, false}}
	r := httptest.NewRecorder()
	e.ServeHTTP(r, httptest.NewRequest("GET", "/api/users/u2", nil))
	if r.Code != 404 || !strings.Contains(r.Body.String(), "relationship_not_found") {
		t.Fatalf("blocked target: HTTP %d %s", r.Code, r.Body.String())
	}

	// Regular user that is not a friend: bio must be hidden.
	singleRoundTripCanned.values = [][]driver.Value{{base[0], base[1], base[2], base[3], base[4], base[5], base[6], base[7], base[8], false, false}}
	r = httptest.NewRecorder()
	e.ServeHTTP(r, httptest.NewRequest("GET", "/api/users/u2", nil))
	if r.Code != 200 {
		t.Fatalf("visible target: HTTP %d %s", r.Code, r.Body.String())
	}
	var visible relationshipUserResponse
	if err := json.Unmarshal(r.Body.Bytes(), &visible); err != nil {
		t.Fatal(err)
	}
	if visible.Bio != "" || visible.ID != "u2" {
		t.Fatalf("non-friend bio not hidden: %#v", visible)
	}

	// Friend: bio is shown.
	singleRoundTripCanned.values = [][]driver.Value{{base[0], base[1], base[2], base[3], base[4], base[5], base[6], base[7], base[8], false, true}}
	r = httptest.NewRecorder()
	e.ServeHTTP(r, httptest.NewRequest("GET", "/api/users/u2", nil))
	var friend relationshipUserResponse
	if err := json.Unmarshal(r.Body.Bytes(), &friend); err != nil {
		t.Fatal(err)
	}
	if friend.Bio != "secret bio" {
		t.Fatalf("friend bio hidden: %#v", friend)
	}
}

func commentColumns() []string {
	return []string{
		"allowed",
		"id", "post_id", "author_id", "body", "created_at", "deleted_at",
		"author.id", "author.username", "author.secret_key_hash", "author.display_name",
		"author.bio", "author.profile_image_id", "author.region", "author.account_type", "author.status",
	}
}

func TestGetCommentsHandlerCollapsedQuery(t *testing.T) {
	db := singleRoundTripDB(t)
	e := echo.New()
	e.HTTPErrorHandler = APIErrorHandler
	e.GET("/api/posts/:postId/comments", func(c *echo.Context) error {
		c.Set("session", &Session{UserID: "viewer"})
		return getCommentsHandler(&HandlerContext{DB: db})(c)
	})
	singleRoundTripCanned.columns = commentColumns()

	// Denied: the permission CTE contributes a single dummy row, no comment data.
	singleRoundTripCanned.values = [][]driver.Value{{
		false, "", "", "", "", time.Unix(0, 0).UTC(), nil,
		"", "", "", "", "", nil, "", "", "",
	}}
	r := httptest.NewRecorder()
	e.ServeHTTP(r, httptest.NewRequest("GET", "/api/posts/p1/comments", nil))
	if r.Code != 403 || !strings.Contains(r.Body.String(), "interaction_forbidden") {
		t.Fatalf("denied: HTTP %d %s", r.Code, r.Body.String())
	}

	// Allowed with two authored comments.
	singleRoundTripCanned.values = [][]driver.Value{
		{true, "c1", "p1", "author-a", "first", time.Unix(1, 0).UTC(), nil, "author-a", "name-a", strings.Repeat("00", 32), "A", "", nil, "asia", "regular", "active"},
		{true, "c2", "p1", "author-b", "second", time.Unix(2, 0).UTC(), nil, "author-b", "name-b", strings.Repeat("00", 32), "B", "", nil, "asia", "regular", "active"},
	}
	r = httptest.NewRecorder()
	e.ServeHTTP(r, httptest.NewRequest("GET", "/api/posts/p1/comments", nil))
	if r.Code != 200 {
		t.Fatalf("allowed: HTTP %d %s", r.Code, r.Body.String())
	}
	var response getCommentsResponse
	if err := json.Unmarshal(r.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if len(response.Items) != 2 || response.Items[0].ID != "c1" || response.Items[0].Author.DisplayName != "A" || response.Items[1].Author.Username != "name-b" {
		t.Fatalf("unexpected items: %#v", response.Items)
	}

	// Allowed but empty: only the dummy row remains and is dropped.
	singleRoundTripCanned.values = [][]driver.Value{{
		true, "", "", "", "", time.Unix(0, 0).UTC(), nil,
		"", "", "", "", "", nil, "", "", "",
	}}
	r = httptest.NewRecorder()
	e.ServeHTTP(r, httptest.NewRequest("GET", "/api/posts/p1/comments", nil))
	var empty getCommentsResponse
	if err := json.Unmarshal(r.Body.Bytes(), &empty); err != nil {
		t.Fatal(err)
	}
	if len(empty.Items) != 0 || empty.HasMore {
		t.Fatalf("expected empty page: %#v", empty)
	}
}

func realMojiColumns() []string {
	return []string{
		"allowed",
		"id", "post_id", "reactor_id", "image_id", "reaction_type", "created_at", "replaced_at", "deleted_at",
		"reactor.id", "reactor.username", "reactor.secret_key_hash", "reactor.display_name",
		"reactor.bio", "reactor.profile_image_id", "reactor.region", "reactor.account_type", "reactor.status",
	}
}

func TestGetRealMojisHandlerCollapsedQuery(t *testing.T) {
	db := singleRoundTripDB(t)
	e := echo.New()
	e.HTTPErrorHandler = APIErrorHandler
	e.GET("/api/posts/:postId/realmojis", func(c *echo.Context) error {
		c.Set("session", &Session{UserID: "viewer"})
		return getRealMojisHandler(&HandlerContext{DB: db})(c)
	})
	singleRoundTripCanned.columns = realMojiColumns()

	// Denied dummy row.
	singleRoundTripCanned.values = [][]driver.Value{{
		false, "", "", "", "", "", time.Unix(0, 0).UTC(), nil, nil,
		"", "", "", "", "", nil, "", "", "",
	}}
	r := httptest.NewRecorder()
	e.ServeHTTP(r, httptest.NewRequest("GET", "/api/posts/p1/realmojis", nil))
	if r.Code != 403 || !strings.Contains(r.Body.String(), "interaction_forbidden") {
		t.Fatalf("denied: HTTP %d %s", r.Code, r.Body.String())
	}

	// Allowed with one realmoji.
	singleRoundTripCanned.values = [][]driver.Value{
		{true, "r1", "p1", "reactor-a", "img-1", "heart", time.Unix(3, 0).UTC(), nil, nil, "reactor-a", "name-a", strings.Repeat("00", 32), "A", "", nil, "asia", "regular", "active"},
	}
	r = httptest.NewRecorder()
	e.ServeHTTP(r, httptest.NewRequest("GET", "/api/posts/p1/realmojis", nil))
	if r.Code != 200 {
		t.Fatalf("allowed: HTTP %d %s", r.Code, r.Body.String())
	}
	var response getRealMojisResponse
	if err := json.Unmarshal(r.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if len(response.Items) != 1 || response.Items[0].ID != "r1" || response.Items[0].ReactionType != "heart" || response.Items[0].Reactor.ID != "reactor-a" {
		t.Fatalf("unexpected items: %#v", response.Items)
	}
}
