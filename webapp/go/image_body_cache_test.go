package isureal

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jmoiron/sqlx"
	"github.com/labstack/echo/v5"
)

func TestImageBodyCacheLimitsAndEpoch(t *testing.T) {
	c := newImageBodyCache(6, 2)
	a, b, d := imageBodyKey{ID: "a"}, imageBodyKey{ID: "b"}, imageBodyKey{ID: "d"}
	body := []byte("abc")
	c.put(1, a, cachedImageBody{Data: body})
	body[0] = 'x'
	c.put(1, b, cachedImageBody{Data: []byte("def")})
	got, ok := c.get(1, a)
	if !ok || string(got.Data) != "abc" {
		t.Fatal("cache does not own stored bytes")
	}
	c.put(1, d, cachedImageBody{Data: []byte("ghi")})
	if _, ok := c.get(1, b); ok {
		t.Fatal("least recently used entry retained")
	}
	c.put(1, b, cachedImageBody{Data: []byte("oversized")})
	if c.bytes != 6 || len(c.entries) != 2 {
		t.Fatal("capacity bound violated")
	}
	if _, ok := c.get(2, a); ok {
		t.Fatal("old epoch hit after initialization")
	}
	c.put(1, a, cachedImageBody{Data: []byte("old")})
	if _, ok := c.get(2, a); ok {
		t.Fatal("inflight old epoch repopulated cache")
	}
	c.put(2, a, cachedImageBody{Data: []byte("new")})
	if _, ok := c.get(1, a); ok {
		t.Fatal("old snapshot accessed new epoch")
	}
	c.clear()
	if c.bytes != 0 || len(c.entries) != 0 {
		t.Fatal("clear retained entries")
	}
}
func TestImageBodyCacheConcurrent(t *testing.T) {
	c := newImageBodyCache(256, 8)
	var wg sync.WaitGroup
	for n := range 12 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range 1000 {
				k := imageBodyKey{ID: fmt.Sprint(n, i%16)}
				c.put(int64(i/100), k, cachedImageBody{Data: make([]byte, 32)})
				c.get(int64(i/100), k)
			}
		}()
	}
	wg.Wait()
	if c.bytes > 256 || len(c.entries) > 8 {
		t.Fatal("concurrent capacity violation")
	}
}

type imageTestState struct {
	active, valid            bool
	epoch                    int64
	body                     string
	metadataCalls, bodyCalls int
	key                      string
}

var imageTestCurrent *imageTestState

// Sequential handler test only; each Open retains its own scenario pointer.
type imageTestDriver struct{}
type imageTestConn struct{ state *imageTestState }

func (imageTestDriver) Open(string) (driver.Conn, error) {
	return &imageTestConn{imageTestCurrent}, nil
}
func (c *imageTestConn) Prepare(string) (driver.Stmt, error) { return nil, fmt.Errorf("unsupported") }
func (c *imageTestConn) Close() error                        { return nil }
func (c *imageTestConn) Begin() (driver.Tx, error)           { return nil, fmt.Errorf("unsupported") }
func (c *imageTestConn) QueryContext(_ context.Context, q string, _ []driver.NamedValue) (driver.Rows, error) {
	s := c.state
	if strings.Contains(q, "SELECT i.data") {
		s.bodyCalls++
		return &notificationRows{[]string{"data", "media_type"}, [][]driver.Value{{[]byte(s.body), "image/jpeg"}}}, nil
	}
	s.metadataCalls++
	rows := &notificationRows{columns: []string{"secret_key_hash", "generation", "created_at", "source", "media_type"}}
	if s.active {
		source := ""
		if s.valid {
			source = "post"
		}
		rows.values = [][]driver.Value{{s.key, s.epoch, time.Date(2026, 10, 31, 0, 0, 0, 0, time.UTC), source, "image/jpeg"}}
	}
	return rows, nil
}
func init() { sql.Register("image-body-test", imageTestDriver{}) }
func TestImageHandlerChecksFreshStateOnCacheHits(t *testing.T) {
	imageBodies = newImageBodyCache(128<<20, 4096)
	defer func() { imageBodies = newImageBodyCache(128<<20, 4096) }()
	key := make([]byte, 32)
	key[0] = 123
	s := &imageTestState{active: true, valid: true, epoch: 1, body: "first", key: hex.EncodeToString(key)}
	imageTestCurrent = s
	db, err := sqlx.Open("image-body-test", "")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	e := echo.New()
	e.HTTPErrorHandler = APIErrorHandler
	var abuse any
	handler := getImageHandler(&HandlerContext{DB: db})
	e.GET("/api/images/:imageId", func(c *echo.Context) error { err := handler(c); abuse = c.Get("abuse"); return err })
	exp := time.Now().Add(time.Hour).Unix()
	fetch := func(sig string, expiry int64) (int, string) {
		t.Helper()
		abuse = nil
		r := httptest.NewRecorder()
		e.ServeHTTP(r, httptest.NewRequest("GET", fmt.Sprintf("/api/images/i1?exp=%d&sig=%s", expiry, sig), nil))
		if r.Header().Get("Cache-Control") != "no-store" {
			t.Fatal("cache header changed")
		}
		return r.Code, r.Body.String()
	}
	sig := base64.RawURLEncoding.EncodeToString(imageSignature(key, "i1", exp))
	for range 2 {
		if status, body := fetch(sig, exp); status != 200 || body != "first" {
			t.Fatalf("%d %s", status, body)
		}
	}
	if s.metadataCalls != 2 || s.bodyCalls != 1 {
		t.Fatal("hit must check DB metadata but skip body retrieval")
	}
	s.valid = false
	if status, _ := fetch(sig, exp); status != 403 {
		t.Fatal("deleted post or replaced reaction served from cache")
	}
	badSig := base64.RawURLEncoding.EncodeToString(make([]byte, 32))
	if status, _ := fetch(badSig, exp); status != 403 || abuse != "invalid_signature" {
		t.Fatal("invalid image signature abuse was not recorded")
	}
	s.valid = true
	s.active = false
	if status, _ := fetch(sig, exp); status != 403 {
		t.Fatal("inactive owner served from cache")
	}
	s.active = true
	s.epoch = 2
	s.body = "after reset"
	if status, body := fetch(sig, exp); status != 200 || body != "after reset" {
		t.Fatal("reset returned old bytes")
	}
	if s.bodyCalls != 2 {
		t.Fatal("reset did not miss cache")
	}
	if status, _ := fetch(sig, time.Now().Unix()-1); status != 403 {
		t.Fatal("expired image served")
	}
	s.key = hex.EncodeToString(make([]byte, 32))
	if status, _ := fetch(sig, exp); status != 403 {
		t.Fatal("changed owner signing key ignored")
	}
}
