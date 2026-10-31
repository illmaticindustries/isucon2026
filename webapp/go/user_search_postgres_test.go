package isureal

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"net/url"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/jmoiron/sqlx"
	"github.com/labstack/echo/v5"
	"golang.org/x/text/cases"
	"golang.org/x/text/unicode/norm"
)

// Real PostgreSQL validates the prefilter, block visibility, pgx parameters,
// Unicode normalization, and write paths. Only connection-local TEMP tables are
// created, so this test cannot modify pre-existing application tables.
func TestSearchPostgres(t *testing.T) {
	dsn := os.Getenv("ISUREAL_TEST_DB_DSN")
	if dsn == "" {
		t.Skip("set ISUREAL_TEST_DB_DSN for PostgreSQL integration tests")
	}
	db, err := sqlx.Connect("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	db.MustExec(`CREATE TEMP TABLE users (
 id varchar(36) PRIMARY KEY, username varchar(64) NOT NULL, secret_key_hash text NOT NULL,
 display_name text NOT NULL, bio text NOT NULL DEFAULT '', profile_image_id varchar(36),
 region text NOT NULL DEFAULT 'asia', account_type text NOT NULL DEFAULT 'regular',
 status text NOT NULL DEFAULT 'active', updated_at timestamptz DEFAULT now())`)
	db.MustExec(`CREATE TEMP TABLE block_relations (blocker_id text, blocked_id text)`)
	migration, err := os.ReadFile("../sql/007-user-search.sql")
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if _, err := db.Exec(string(migration)); err != nil {
			t.Fatal(err)
		}
	}
	db.MustExec(`INSERT INTO users(id,username,secret_key_hash,display_name,bio,account_type)
 SELECT 'u'||i, 'user'||lpad(i::text,4,'0'), repeat('00',32),
 CASE i%6 WHEN 0 THEN 'Straße' WHEN 1 THEN 'Café' WHEN 2 THEN 'ος'
 WHEN 3 THEN 'İstanbul' WHEN 4 THEN 'ﬃ %_\' ELSE 'unrelated' END,
 'private bio', CASE WHEN i%4=0 THEN 'certified_public' ELSE 'regular' END
 FROM generate_series(0,599) i`)
	db.MustExec(`INSERT INTO users(id,username,secret_key_hash,display_name) VALUES('viewer','viewer',repeat('00',32),'self')`)
	db.MustExec(`INSERT INTO block_relations SELECT CASE WHEN i%2=0 THEN 'viewer' ELSE 'u'||i END,
 CASE WHEN i%2=0 THEN 'u'||i ELSE 'viewer' END FROM generate_series(0,599,7) i`)
	h := &HandlerContext{DB: db}
	e := echo.New()
	e.HTTPErrorHandler = APIErrorHandler
	e.GET("/api/users", func(c *echo.Context) error {
		c.Set("session", &Session{UserID: "viewer"})
		return searchUsersHandler(h)(c)
	})
	e.PATCH("/api/me", func(c *echo.Context) error {
		c.Set("session", &Session{UserID: "u1"})
		return updateMeHandler(h)(c)
	})
	e.POST("/api/auth/register", registerHandler(h))
	check := func() {
		t.Helper()
		var all []User
		if err := db.Select(&all, `SELECT `+strings.Join(userColumns, ",")+` FROM users WHERE status='active' AND id<>'viewer' ORDER BY username,id`); err != nil {
			t.Fatal(err)
		}
		blocked, err := getBlockedUserIDs(context.Background(), db, "viewer")
		if err != nil {
			t.Fatal(err)
		}
		fold := func(s string) string { return norm.NFC.String(cases.Fold().String(norm.NFC.String(s))) }
		for _, query := range []string{"user", "STRASSE", "CAFÉ", "ΟΣ", "i̇", "ffi", "%", "_", "\\", "\x00", "not-present"} {
			var expected, got []string
			for _, u := range all {
				if !blocked[u.ID] && (strings.Contains(fold(u.Username), fold(query)) || strings.Contains(fold(u.DisplayName), fold(query))) {
					expected = append(expected, u.ID)
				}
			}
			cursor, cursorID := "", ""
			for page := 0; ; page++ {
				if page > 20 {
					t.Fatal("pagination did not finish")
				}
				q := url.Values{"q": {query}, "beforeUsername": {cursor}, "beforeId": {cursorID}}
				r := httptest.NewRecorder()
				e.ServeHTTP(r, httptest.NewRequest("GET", "/api/users?"+q.Encode(), nil))
				if r.Code != 200 {
					t.Fatalf("query %q: %d %s", query, r.Code, r.Body.String())
				}
				var result searchUsersResponse
				if err := json.Unmarshal(r.Body.Bytes(), &result); err != nil {
					t.Fatal(err)
				}
				for _, u := range result.Items {
					got = append(got, u.ID)
					if u.AccountType != "certified_public" && u.Bio != "" {
						t.Fatal("private bio leaked")
					}
				}
				if result.HasMore != (len(expected) > len(got)) {
					t.Fatalf("query %q: incorrect hasMore", query)
				}
				if !result.HasMore {
					break
				}
				last := result.Items[len(result.Items)-1]
				cursor, cursorID = last.Username, last.ID
			}
			if !reflect.DeepEqual(got, expected) {
				t.Fatalf("query %q: got %v want %v", query, got, expected)
			}
		}
	}
	check() // unpopulated derived columns must be safe
	if err := rebuildUserSearchIndex(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	check()
	// The next search must see each committed block change in either direction.
	db.MustExec(`INSERT INTO block_relations VALUES ('viewer','u1'), ('u2','viewer')`)
	check()
	db.MustExec(`DELETE FROM block_relations WHERE (blocker_id,blocked_id) IN (('viewer','u1'),('u2','viewer'))`)
	check()
	var candidates []User
	if err := db.Select(&candidates, searchCandidateQuery, "viewer", "", "", "not-present"); err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 0 {
		t.Fatalf("prefilter transmitted %d unrelated users", len(candidates))
	}
	for _, body := range []string{`{"displayName":"Straße CAFÉ"}`, `{"bio":"new bio","region":"europe"}`} {
		r := httptest.NewRecorder()
		req := httptest.NewRequest("PATCH", "/api/me", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		e.ServeHTTP(r, req)
		if r.Code != 200 {
			t.Fatalf("profile update: %d %s", r.Code, r.Body.String())
		}
	}
	r := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/auth/register", strings.NewReader(`{"username":"new.user","displayName":"Straße %_","region":"asia"}`))
	req.Header.Set("Content-Type", "application/json")
	e.ServeHTTP(r, req)
	if r.Code != 201 {
		t.Fatalf("register: %d %s", r.Code, r.Body.String())
	}
	check()
	// Older binaries / initialization can change source fields without updating
	// the index. This must neither hide new matches nor expose stale matches.
	db.MustExec(`UPDATE users SET display_name='STRASSE _' WHERE id='u2'`)
	db.MustExec(`UPDATE users SET search_display_name_normalized=NULL WHERE id='u3'`)
	db.MustExec(`UPDATE users SET status='deleted' WHERE id='u4'`)
	check()
	if err := rebuildUserSearchIndex(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	check()
	var stale int
	if err := db.Get(&stale, `SELECT count(*) FROM users WHERE search_display_name_source IS DISTINCT FROM display_name OR search_display_name_normalized IS NULL`); err != nil {
		t.Fatal(err)
	}
	if stale != 0 {
		t.Fatal(fmt.Sprintf("%d stale index entries", stale))
	}
}
