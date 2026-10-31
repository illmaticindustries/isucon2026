package isureal

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"

	"github.com/jmoiron/sqlx"
	"github.com/labstack/echo/v5"
	"golang.org/x/text/cases"
	"golang.org/x/text/unicode/norm"
)

type searchTestState struct {
	users         []User
	blocked       map[string]bool
	rows          int
	queries       int
	fetches       int
	reads, closes int
	failRead      bool
}

var searchTestCurrent *searchTestState

type searchTestDriver struct{}
type searchTestConn struct{ state *searchTestState }

func (searchTestDriver) Open(string) (driver.Conn, error) {
	return &searchTestConn{searchTestCurrent}, nil
}
func (c *searchTestConn) Prepare(string) (driver.Stmt, error) { return nil, fmt.Errorf("unsupported") }
func (c *searchTestConn) Close() error                        { return nil }
func (c *searchTestConn) Begin() (driver.Tx, error)           { return searchTestTx{}, nil }
func (c *searchTestConn) BeginTx(context.Context, driver.TxOptions) (driver.Tx, error) {
	return searchTestTx{}, nil
}

// Accept []string for "id = ANY($1)" as pgx/stdlib does.
func (c *searchTestConn) CheckNamedValue(*driver.NamedValue) error { return nil }

type searchTestTx struct{}

func (searchTestTx) Commit() error   { return nil }
func (searchTestTx) Rollback() error { return nil }

type searchTestRows struct {
	notificationRows
	state *searchTestState
}

func (r *searchTestRows) Next(v []driver.Value) error {
	if r.state.failRead && r.state.reads == 10 {
		return fmt.Errorf("injected row read error")
	}
	err := r.notificationRows.Next(v)
	if err == nil {
		r.state.reads++
	}
	return err
}
func (r *searchTestRows) Close() error { r.state.closes++; return nil }
func (c *searchTestConn) QueryContext(_ context.Context, q string, args []driver.NamedValue) (driver.Rows, error) {
	s := c.state
	if strings.Contains(q, "block_relations") {
		r := &notificationRows{columns: []string{"user_id"}}
		for id := range s.blocked {
			r.values = append(r.values, []driver.Value{id})
		}
		return r, nil
	}
	if strings.Contains(q, "id = ANY($1)") {
		s.fetches++
		ids := args[0].Value.([]string)
		r := &notificationRows{columns: userColumns}
		// Return rows in reverse so the handler must restore the scan order.
		for i := len(ids) - 1; i >= 0; i-- {
			for _, u := range s.users {
				if u.ID == ids[i] {
					r.values = append(r.values, []driver.Value{u.ID, u.Username, u.SecretKeyHash, u.DisplayName, u.Bio, nil, u.Region, u.AccountType, u.Status})
				}
			}
		}
		return r, nil
	}
	s.queries++
	r := &notificationRows{columns: []string{"id", "username", "display_name"}}
	if !strings.Contains(q, "SELECT id, username, display_name FROM users") || !strings.Contains(q, "ORDER BY username, id") {
		return nil, fmt.Errorf("unexpected query: %s", q)
	}
	cursor, id := args[1].Value.(string), args[2].Value.(string)
	for _, u := range s.users {
		if u.Status != "active" || u.ID == args[0].Value || (cursor != "" && (u.Username < cursor || (u.Username == cursor && u.ID <= id))) {
			continue
		}
		r.values = append(r.values, []driver.Value{u.ID, u.Username, u.DisplayName})
	}
	s.rows += len(r.values)
	return &searchTestRows{*r, s}, nil
}
func init() { sql.Register("search-pagination-test", searchTestDriver{}) }

func TestSearchPaginationMatchesFullScan(t *testing.T) {
	for _, query := range []string{"user", "STRASSE", "CAFE\u0301", "ΟΣ", "missing"} {
		for _, count := range []int{0, 50, 51, 200, 600} {
			t.Run(fmt.Sprintf("%s/%d", query, count), func(t *testing.T) {
				s := &searchTestState{blocked: map[string]bool{}}
				for i := range count {
					id := fmt.Sprintf("user%04d", i)
					name := "unrelated"
					if i >= 190 {
						name = []string{"Straße", "Café", "ος"}[i%3]
					}
					account := "normal"
					if i%4 == 0 {
						account = "certified_public"
					}
					s.users = append(s.users, User{ID: id, Username: id, DisplayName: name, Bio: "private bio", SecretKeyHash: strings.Repeat("00", 32), Status: "active", Region: "asia", AccountType: account})
					if i%7 == 0 {
						s.blocked[id] = true
					}
				}
				searchTestCurrent = s
				db, err := sqlx.Open("search-pagination-test", "")
				if err != nil {
					t.Fatal(err)
				}
				defer db.Close()
				e := echo.New()
				e.HTTPErrorHandler = APIErrorHandler
				e.GET("/api/users", func(c *echo.Context) error {
					c.Set("session", &Session{UserID: "viewer"})
					return searchUsersHandler(&HandlerContext{DB: db})(c)
				})
				// Reference is the original full scan with independent NFC/case folding.
				normalize := func(v string) string { return norm.NFC.String(cases.Fold().String(norm.NFC.String(v))) }
				var expected []string
				for _, u := range s.users {
					if !s.blocked[u.ID] && (strings.Contains(normalize(u.Username), normalize(query)) || strings.Contains(normalize(u.DisplayName), normalize(query))) {
						expected = append(expected, u.ID)
					}
				}
				var actual []string
				cursor := ""
				cursorID := ""
				pages := 0
				for page := 0; ; page++ {
					pages++
					if page > 20 {
						t.Fatal("pagination did not finish")
					}
					values := url.Values{"q": {query}}
					if cursor != "" {
						values.Set("beforeUsername", cursor)
						values.Set("beforeId", cursorID)
					}
					r := httptest.NewRecorder()
					e.ServeHTTP(r, httptest.NewRequest("GET", "/api/users?"+values.Encode(), nil))
					if r.Code != 200 {
						t.Fatalf("%d: %s", r.Code, r.Body.String())
					}
					var response searchUsersResponse
					if err := json.Unmarshal(r.Body.Bytes(), &response); err != nil {
						t.Fatal(err)
					}
					if len(response.Items) > 50 {
						t.Fatal("page too large")
					}
					for _, u := range response.Items {
						actual = append(actual, u.ID)
						if u.AccountType != "certified_public" && u.Bio != "" {
							t.Fatal("private bio leaked")
						}
					}
					if response.HasMore != (len(expected) > len(actual)) {
						t.Fatal("incorrect hasMore")
					}
					if !response.HasMore {
						break
					}
					last := response.Items[len(response.Items)-1]
					cursor, cursorID = last.Username, last.ID
				}
				if !reflect.DeepEqual(actual, expected) {
					t.Fatalf("got %v want %v", actual, expected)
				}
				if s.queries != pages {
					t.Fatalf("%d scan queries for %d pages", s.queries, pages)
				}
				if s.fetches > pages {
					t.Fatalf("%d row fetches for %d pages", s.fetches, pages)
				}
			})
		}
	}
}

func TestSearchClosesRowsAndPropagatesReadErrors(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(fmt.Sprint(fail), func(t *testing.T) {
			s := &searchTestState{blocked: map[string]bool{}, failRead: fail}
			for i := range 600 {
				id := fmt.Sprintf("user%04d", i)
				s.users = append(s.users, User{ID: id, Username: id, SecretKeyHash: strings.Repeat("00", 32), Status: "active", AccountType: "normal"})
			}
			searchTestCurrent = s
			db, err := sqlx.Open("search-pagination-test", "")
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			e := echo.New()
			e.HTTPErrorHandler = APIErrorHandler
			e.GET("/api/users", func(c *echo.Context) error {
				c.Set("session", &Session{UserID: "viewer"})
				return searchUsersHandler(&HandlerContext{DB: db})(c)
			})
			r := httptest.NewRecorder()
			e.ServeHTTP(r, httptest.NewRequest("GET", "/api/users?q=user", nil))
			if fail {
				if r.Code != 500 {
					t.Fatalf("read error became HTTP %d", r.Code)
				}
			} else {
				if r.Code != 200 || s.reads != 51 {
					t.Fatalf("status=%d scanned=%d", r.Code, s.reads)
				}
			}
			if s.queries != 1 || s.closes != 1 {
				t.Fatalf("queries=%d closes=%d", s.queries, s.closes)
			}
		})
	}
}
