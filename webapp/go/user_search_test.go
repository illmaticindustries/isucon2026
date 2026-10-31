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
	users   []User
	blocked map[string]bool
	rows    int
}

var searchTestCurrent *searchTestState

type searchTestDriver struct{}
type searchTestConn struct{ state *searchTestState }

func (searchTestDriver) Open(string) (driver.Conn, error) {
	return &searchTestConn{searchTestCurrent}, nil
}
func (c *searchTestConn) Prepare(string) (driver.Stmt, error) { return nil, fmt.Errorf("unsupported") }
func (c *searchTestConn) Close() error                        { return nil }
func (c *searchTestConn) Begin() (driver.Tx, error)           { return nil, fmt.Errorf("unsupported") }
func (c *searchTestConn) QueryContext(_ context.Context, q string, args []driver.NamedValue) (driver.Rows, error) {
	s := c.state
	if strings.Contains(q, "block_relations") {
		r := &notificationRows{columns: []string{"user_id"}}
		for id := range s.blocked {
			r.values = append(r.values, []driver.Value{id})
		}
		return r, nil
	}
	r := &notificationRows{columns: userColumns}
	if !strings.Contains(q, "ORDER BY username, id") {
		return nil, fmt.Errorf("unexpected query: %s", q)
	}
	cursor, id := args[1].Value.(string), args[2].Value.(string)
	for _, u := range s.users {
		if u.Status != "active" || u.ID == args[0].Value || (cursor != "" && (u.Username < cursor || (u.Username == cursor && u.ID <= id))) {
			continue
		}
		r.values = append(r.values, []driver.Value{u.ID, u.Username, u.SecretKeyHash, u.DisplayName, u.Bio, nil, u.Region, u.AccountType, u.Status})
		if strings.Contains(q, "LIMIT 200") && len(r.values) == 200 {
			break
		}
	}
	s.rows += len(r.values)
	return r, nil
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
				for page := 0; ; page++ {
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
				if query == "user" && count == 600 && s.rows >= 600*10 {
					t.Fatal("pagination repeatedly fetched entire table")
				}
			})
		}
	}
}
