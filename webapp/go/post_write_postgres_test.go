package isureal

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"image"
	"image/jpeg"
	"mime/multipart"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/jmoiron/sqlx"
	"github.com/labstack/echo/v5"
)

func isolatedWriteTestDB(t *testing.T) *sqlx.DB {
	t.Helper()
	dsn := os.Getenv("ISUREAL_TEST_DB_DSN")
	if dsn == "" {
		t.Skip("requires PostgreSQL")
	}
	admin, err := sqlx.Connect("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { admin.Close() })
	name := "post_test_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	admin.MustExec("CREATE SCHEMA " + name)
	t.Cleanup(func() { admin.MustExec("DROP SCHEMA " + name + " CASCADE") })
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	cfg.RuntimeParams["search_path"] = name
	cfg.RuntimeParams["application_name"] = name
	db := sqlx.NewDb(stdlib.OpenDB(*cfg), "pgx")
	t.Cleanup(func() { db.Close() })
	var schema string
	if err := db.Get(&schema, "SELECT current_schema()"); err != nil || schema != name {
		t.Fatalf("test schema: %q %v", schema, err)
	}
	return db
}

// Two independent connections must observe the winner after waiting for the
// same author's lock. Using a shared, isolated test schema exercises real row
// locks, the partitioned image table, and the HTTP handler together.
func TestPostSlotAfterLockWait(t *testing.T) {
	db := isolatedWriteTestDB(t)
	for _, file := range []string{"001-schema.sql", "005-image-cache.sql"} {
		contents, err := os.ReadFile(filepath.Join("../sql", file))
		if err != nil {
			t.Fatal(err)
		}
		db.MustExec(string(contents))
	}
	db.MustExec(`INSERT INTO users(id,username,secret_key_hash,display_name,region) VALUES('author','author',repeat('12',32),'Author','asia');
 INSERT INTO posting_windows(id,region,local_date,starts_at,on_time_deadline)
 VALUES ('asia','asia',CURRENT_DATE,now()-interval '1 minute',now()+interval '1 hour'),
 ('europe','europe',CURRENT_DATE,now()-interval '1 minute',now()+interval '1 hour')`)
	files := &imageFileStore{root: t.TempDir()}
	h := createPostHandler(&HandlerContext{DB: db, ImageFiles: files})
	e := echo.New()
	e.HTTPErrorHandler = APIErrorHandler
	e.POST("/api/posts", func(c *echo.Context) error { c.Set("session", &Session{UserID: "author"}); return h(c) })
	var photo, form bytes.Buffer
	if err := jpeg.Encode(&photo, image.NewGray(image.Rect(0, 0, 8, 8)), nil); err != nil {
		t.Fatal(err)
	}
	writer := multipart.NewWriter(&form)
	for k, v := range map[string]string{"caption": "test caption", "visibility": "public", "location_kind": "exact", "latitude": "35.123", "longitude": "139.456"} {
		if err := writer.WriteField(k, v); err != nil {
			t.Fatal(err)
		}
	}
	part, err := writer.CreateFormFile("image", "test.jpg")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = part.Write(photo.Bytes()); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	post := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest("POST", "/api/posts", bytes.NewReader(form.Bytes()))
		req.Header.Set("Content-Type", writer.FormDataContentType())
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, req)
		return rec
	}
	gate := db.MustBegin()
	defer gate.Rollback()
	gate.MustExec(`SELECT id FROM users WHERE id='author' FOR NO KEY UPDATE`)
	results := make(chan *httptest.ResponseRecorder, 2)
	for range 2 {
		go func() { results <- post() }()
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		var waiting int
		if err := db.Get(&waiting, `SELECT count(*) FROM pg_stat_activity WHERE application_name=current_schema() AND wait_event_type='Lock'`); err != nil {
			t.Fatal(err)
		}
		if waiting == 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("both requests did not reach author lock")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := gate.Commit(); err != nil {
		t.Fatal(err)
	}
	var codes []int
	var created createPostResponse
	for range 2 {
		result := <-results
		codes = append(codes, result.Code)
		if result.Code == 201 {
			if err := json.Unmarshal(result.Body.Bytes(), &created); err != nil {
				t.Fatal(err)
			}
		}
	}
	sort.Ints(codes)
	if fmt.Sprint(codes) != "[201 409]" {
		t.Fatalf("same author and date: %v", codes)
	}
	var stored struct {
		ID        string    `db:"id"`
		CreatedAt time.Time `db:"created_at"`
		PostedAt  time.Time `db:"posted_at"`
		Data      []byte    `db:"data"`
	}
	if err := db.Get(&stored, `SELECT i.id,i.created_at,p.posted_at,i.data FROM posts p JOIN image_objects i ON i.id=p.image_id WHERE p.id=$1`, created.ID); err != nil {
		t.Fatal(err)
	}
	if !stored.CreatedAt.Equal(created.PostedAt) || !stored.PostedAt.Equal(created.PostedAt) || !bytes.Equal(stored.Data, photo.Bytes()) ||
		created.Caption != "test caption" || created.Author.ID != "author" || created.PostingWindowID != "asia" || !created.OnTime ||
		created.Location.Latitude == nil || *created.Location.Latitude != 35.123 || !strings.Contains(created.ImageURL, "/api/images/"+stored.ID+"?") {
		t.Fatal("response, partition key or image bytes differ from stored values")
	}
	if !files.exists(fileImageKey(stored.ID, stored.CreatedAt, "image/jpeg", 1)) {
		t.Fatal("committed file was not warmed")
	}
	db.MustExec("UPDATE users SET region='europe' WHERE id='author'")
	if rec := post(); rec.Code != 409 {
		t.Fatalf("active post after region change: %d", rec.Code)
	}
	db.MustExec("UPDATE posts SET deleted_at=now() WHERE deleted_at IS NULL")
	if rec := post(); rec.Code != 201 {
		t.Fatalf("first replacement: %d", rec.Code)
	}
	db.MustExec("UPDATE posts SET deleted_at=now() WHERE deleted_at IS NULL")
	if rec := post(); rec.Code != 409 {
		t.Fatalf("two deletions across regions: %d", rec.Code)
	}
	db.MustExec("UPDATE posting_windows SET ended_at=now() WHERE region='europe'")
	if rec := post(); rec.Code != 409 {
		t.Fatalf("closed window: %d", rec.Code)
	}
	db.MustExec(`INSERT INTO posting_windows(id,region,local_date,starts_at,on_time_deadline) VALUES('tomorrow','europe',CURRENT_DATE+1,now()-interval '2 seconds',now()-interval '1 second');
 ALTER TABLE posts ADD CONSTRAINT test_insert_failure CHECK (posting_window_id<>'tomorrow')`)
	if rec := post(); rec.Code != http.StatusInternalServerError {
		t.Fatalf("expected storage failure: %d", rec.Code)
	}
	var count int
	if err := db.Get(&count, "SELECT count(*) FROM image_objects"); err != nil || count != 2 {
		t.Fatalf("orphan images after failed insert: %d %v", count, err)
	}
	db.MustExec(`ALTER TABLE posts DROP CONSTRAINT test_insert_failure;
		UPDATE users SET secret_key_hash='broken' WHERE id='author'`)
	if rec := post(); rec.Code != http.StatusInternalServerError {
		t.Fatalf("invalid stored signing key: %d", rec.Code)
	}
	if err := db.Get(&count, "SELECT count(*) FROM image_objects"); err != nil || count != 2 {
		t.Fatalf("committed a photo that could not be signed: %d %v", count, err)
	}
	db.MustExec(`UPDATE users SET secret_key_hash=repeat('12',32) WHERE id='author';
		UPDATE posting_windows SET starts_at=clock_timestamp()+interval '1 hour' WHERE id='tomorrow'`)
	if rec := post(); rec.Code != http.StatusConflict {
		t.Fatalf("future window accepted a post: %d", rec.Code)
	}
}

type countPostWrites struct {
	net.Conn
	writes *atomic.Int64
}

func (c countPostWrites) Write(p []byte) (int, error) {
	c.writes.Add(1)
	return c.Conn.Write(p)
}

// Count PostgreSQL protocol writes on a warmed connection in CI. A small
// JPEG fits in a single packet; the test exercises the actual write path.
func TestWarmPostDatabaseExchange(t *testing.T) {
	db := isolatedWriteTestDB(t)
	schema, err := os.ReadFile("../sql/001-schema.sql")
	if err != nil {
		t.Fatal(err)
	}
	db.MustExec(string(schema))
	db.MustExec(`INSERT INTO users(id,username,secret_key_hash,display_name,region) VALUES
		('one','one',repeat('12',32),'One','asia'),('two','two',repeat('34',32),'Two','asia'),
		('three','three',repeat('56',32),'Three','asia');
		INSERT INTO posting_windows(id,region,local_date,starts_at,on_time_deadline)
		VALUES('today','asia',CURRENT_DATE,now()-interval '1 minute',now()+interval '1 hour')`)
	cfg, err := pgx.ParseConfig(os.Getenv("ISUREAL_TEST_DB_DSN"))
	if err != nil {
		t.Fatal(err)
	}
	var searchPath string
	if err := db.Get(&searchPath, "SELECT current_schema()"); err != nil {
		t.Fatal(err)
	}
	cfg.RuntimeParams["search_path"] = searchPath
	dial := cfg.DialFunc
	var writes atomic.Int64
	cfg.DialFunc = func(ctx context.Context, network, addr string) (net.Conn, error) {
		c, err := dial(ctx, network, addr)
		if err != nil {
			return nil, err
		}
		return countPostWrites{c, &writes}, nil
	}
	measured := sqlx.NewDb(stdlib.OpenDB(*cfg), "pgx")
	defer measured.Close()
	measured.SetMaxOpenConns(1)
	var imageBytes bytes.Buffer
	if err := jpeg.Encode(&imageBytes, image.NewGray(image.Rect(0, 0, 8, 8)), nil); err != nil {
		t.Fatal(err)
	}
	// The driver also checks the connection on its first return from the
	// pool (or after idling). Include and report that real exchange before
	// measuring a connection that's already been returned and checked.
	for _, id := range []string{"one", "two", "three"} {
		writes.Store(0)
		if _, _, err := storePhotoPost(t.Context(), measured, id, createPostRequest{Visibility: "public"}, PostLocation{Kind: "none"}, imageBytes.Bytes()); err != nil {
			t.Fatal(err)
		}
		t.Logf("post %s: %d protocol writes", id, writes.Load())
	}
	if got := writes.Load(); got != 1 {
		t.Fatalf("warmed post used %d protocol writes, want one batch", got)
	}
}

func TestRegisterConcurrentUsername(t *testing.T) {
	db := isolatedWriteTestDB(t)
	for _, name := range []string{"001-schema.sql", "007-user-search.sql"} {
		data, err := os.ReadFile(filepath.Join("../sql", name))
		if err != nil {
			t.Fatal(err)
		}
		db.MustExec(string(data))
	}
	e := echo.New()
	e.HTTPErrorHandler = APIErrorHandler
	e.POST("/api/auth/register", registerHandler(&HandlerContext{DB: db}))
	gate := db.MustBegin()
	defer gate.Rollback()
	gate.MustExec("SELECT pg_advisory_xact_lock(hashtextextended('same',0))")
	results := make(chan int, 2)
	for range 2 {
		go func() {
			r := httptest.NewRequest("POST", "/api/auth/register", strings.NewReader(`{"username":"same","displayName":"Same","region":"asia"}`))
			r.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			e.ServeHTTP(w, r)
			results <- w.Code
		}()
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		var n int
		if err := db.Get(&n, "SELECT count(*) FROM pg_stat_activity WHERE application_name=current_schema() AND wait_event='advisory'"); err != nil {
			t.Fatal(err)
		}
		if n == 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("registrations did not reach the username lock")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := gate.Commit(); err != nil {
		t.Fatal(err)
	}
	codes := []int{<-results, <-results}
	sort.Ints(codes)
	if codes[0] != 201 || codes[1] != 409 {
		t.Fatalf("registration statuses %v", codes)
	}
	var count int
	if err := db.Get(&count, "SELECT count(*) FROM users WHERE username='same'"); err != nil || count != 1 {
		t.Fatalf("rows=%d err=%v", count, err)
	}
}
