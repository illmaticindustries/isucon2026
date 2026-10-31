package isureal

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/jmoiron/sqlx"
	"github.com/labstack/echo/v5"
)

// Run the real HTTP writer in a different OS process: it cannot directly
// clear the reader's maps, even accidentally.
func TestRegionalWriterProcess(t *testing.T) {
	schema := os.Getenv("ISUREAL_TEST_WRITER_SCHEMA")
	if schema == "" {
		t.Skip("child process of the regional integration test")
	}
	cfg, err := pgx.ParseConfig(os.Getenv("ISUREAL_TEST_DB_DSN"))
	if err != nil {
		t.Fatal(err)
	}
	cfg.RuntimeParams["search_path"] = schema
	db := sqlx.NewDb(stdlib.OpenDB(*cfg), "pgx")
	defer db.Close()
	file := os.NewFile(3, "writer-listener")
	defer file.Close()
	listener, err := net.FileListener(file)
	if err != nil {
		t.Fatal(err)
	}
	t.Fatal(http.Serve(listener, NewServerWithDB(&Config{Region: "asia", LogLevel: "error"}, db)))
}

func regionalWriter(t *testing.T, db *sqlx.DB) string {
	t.Helper()
	var schema string
	if err := db.Get(&schema, "SELECT current_schema()"); err != nil {
		t.Fatal(err)
	}
	ln, err := net.ListenTCP("tcp4", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	file, err := ln.File()
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	cmd := exec.Command(os.Args[0], "-test.run=^TestRegionalWriterProcess$")
	cmd.ExtraFiles = []*os.File{file}
	cmd.Env = append(os.Environ(), "ISUREAL_TEST_WRITER_SCHEMA="+schema)
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	return "http://" + ln.Addr().String()
}

// The two readers have separate authorization caches and image directories.
// Hold their only DB connection while checking warm responses.
func TestRegionalReadsAfterCrossRegionWrites(t *testing.T) {
	db := isolatedWriteTestDB(t)
	for _, name := range []string{"001-schema.sql", "010-regional-read-leases.sql", "010-regional-read-leases.sql"} {
		s, err := os.ReadFile(filepath.Join("../sql", name))
		if err != nil {
			t.Fatal(err)
		}
		db.MustExec(string(s))
	}
	photo := []byte{0xff, 0xd8, 0xff, 0x12, 0xff, 0xd9}
	for _, r := range []string{"asia", "europe", "america"} {
		db.MustExec(`INSERT INTO users(id,username,secret_key_hash,display_name,region) VALUES($1::text,$1::text,repeat('17',32),$1::text,$1::text)`, r)
		digest := sha256.Sum256([]byte("token-" + r))
		db.MustExec(`INSERT INTO login_sessions(id,user_id,token_hash) VALUES($1::text,$1::text,$2)`, r, hex.EncodeToString(digest[:]))
		db.MustExec(`INSERT INTO posting_windows(id,region,local_date,starts_at,on_time_deadline) VALUES($1::text,$1::text,CURRENT_DATE,now(),now())`, r)
		db.MustExec(`INSERT INTO image_objects(id,owner_user_id,data,media_type) VALUES('photo-'||$1::text,$1::text,$2,'image/jpeg')`, r, photo)
		db.MustExec(`INSERT INTO posts(id,author_id,posting_window_id,image_id,visibility,location_kind,posted_at) VALUES($1::text,$1::text,$1::text,'photo-'||$1::text,'friends','none',now())`, r)
	}
	connect := func(region string) {
		db.MustExec(`INSERT INTO friendships(id,user_low_id,user_high_id) VALUES($1::text,least('asia',$1::text),greatest('asia',$1::text))`, region)
	}
	connect("america")
	connect("europe")
	asia := regionalWriter(t, db)
	writeBlock := func(method, reg string) {
		t.Helper()
		req, err := http.NewRequest(method, asia+"/api/blocks/"+reg, nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer token-asia")
		client := http.Client{Timeout: 5 * time.Second}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusNoContent {
			t.Fatalf("%s block %s: HTTP %d", method, reg, resp.StatusCode)
		}
	}
	regions := map[string]*echo.Echo{}
	roots := map[string]string{}
	for _, reg := range []string{"europe", "america"} {
		roots[reg] = t.TempDir()
		regions[reg] = NewServerWithDB(&Config{Region: reg, LogLevel: "error", ImageFiles: &imageFileStore{root: roots[reg]}}, db)
	}
	request := func(e *echo.Echo, method, path, actor string, timeout time.Duration) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, nil)
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		req.Header.Set("X-Client-IP", "198.51.100.25")
		req.Header.Set("Authorization", "Bearer token-"+actor)
		req.Header.Set("X-Isureal-Image-Accel", "v1")
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, req.WithContext(ctx))
		return rec
	}
	const feed = "/api/feeds/friends"
	checkFeed := func(reg string, want int, limit time.Duration) getFeedResponse {
		t.Helper()
		rec := request(regions[reg], "GET", feed, reg, limit)
		var out getFeedResponse
		if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &out) != nil || len(out.Items) != want {
			t.Fatalf("%s feed want %d, got %d %s", reg, want, rec.Code, rec.Body.String())
		}
		if want == 1 && out.Items[0].Author.ID != "asia" {
			t.Fatal("wrong author")
		}
		return out
	}
	const clientIP = "198.51.100.25"
	var url string
	waitFills := func(before uint64, count uint64) {
		t.Helper()
		deadline := time.Now().Add(time.Second)
		for regionalReadCounts.imageFills.Load() < before+count {
			if time.Now().After(deadline) {
				t.Fatal("image authorization was not populated")
			}
			time.Sleep(time.Millisecond)
		}
	}
	checkImage := func(reg string, limit time.Duration) {
		t.Helper()
		rec := request(regions[reg], "GET", url, reg, limit)
		key, ok := strings.CutPrefix(rec.Header().Get("X-Accel-Redirect"), imageFilePrefix)
		if rec.Code != 200 || !ok || rec.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("image status %d redirect %t", rec.Code, ok)
		}
		got, err := os.ReadFile(filepath.Join(roots[reg], key))
		if err != nil || !bytes.Equal(got, photo) {
			t.Fatal("local file bytes differ", err)
		}
	}
	before := regionalReadCounts.imageFills.Load()
	db.SetMaxOpenConns(1)
	for _, reg := range []string{"europe", "america"} {
		url = checkFeed(reg, 1, 5*time.Second).Items[0].ImageURL
		checkImage(reg, 5*time.Second)
	}
	waitFills(before, 2)
	conn, err := db.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	func() {
		defer conn.Close()
		for _, reg := range []string{"europe", "america"} {
			checkFeed(reg, 1, 100*time.Millisecond)
			checkImage(reg, 100*time.Millisecond)
		}
	}()
	db.SetMaxOpenConns(8)
	if r := request(regions["europe"], "GET", feed, "america", time.Second); r.Code != 403 {
		t.Fatalf("wrong region: %d", r.Code)
	}
	if r := request(regions["america"], "GET", url+"&sig=wrong", "america", time.Second); r.Code != 403 {
		t.Fatalf("duplicate signature: %d", r.Code)
	}
	for _, reg := range []string{"europe", "america"} {
		checkFeed(reg, 1, 3*time.Second)
		hits := regionalReadCounts.feedHits.Load()
		checkFeed(reg, 1, time.Second)
		if regionalReadCounts.feedHits.Load() != hits+1 {
			t.Fatal("pre-block request did not use a warm feed", reg)
		}
		// Exercise the public HTTP writer in the other region. No sleep or
		// replication polling is allowed between its 204 and this read.
		writeBlock("PUT", reg)
		checkFeed(reg, 0, 3*time.Second)
		writeBlock("DELETE", reg)
		checkFeed(reg, 0, 3*time.Second) // unblock alone does not restore friendship
		connect(reg)
		checkFeed(reg, 1, 3*time.Second)
	}
	// A writer that already holds the slot must stop a fresh regional process
	// from caching a still-visible, precommit feed. Rollback restores grants.
	spanning := db.MustBegin()
	defer spanning.Rollback()
	spanning.MustExec("UPDATE posts SET caption='uncommitted' WHERE id='asia'")
	var granted bool
	hash := sha256.Sum256([]byte("token-europe"))
	if err := db.Get(&granted, `SELECT leased FROM regional_feed_grant($1,'europe',$2)`, hex.EncodeToString(hash[:]), clientIP); err != nil || granted {
		t.Fatal("grant escaped an uncommitted writer", err)
	}
	if page := checkFeed("europe", 1, time.Second); page.Items[0].Caption == "uncommitted" {
		t.Fatal("dirty read")
	}
	if err := spanning.Rollback(); err != nil {
		t.Fatal(err)
	}
	// The replica-mode reset has to drain BOTH regions' grants even though
	// ordinary DML triggers are disabled by the official reset script.
	for _, reg := range []string{"europe", "america"} {
		checkImage(reg, 2*time.Second)
	}
	epoch := db.MustBegin()
	epoch.MustExec("SET LOCAL session_replication_role=replica")
	epoch.MustExec("UPDATE image_cache_generation SET generation=generation+1")
	if err := epoch.Commit(); err != nil {
		t.Fatal(err)
	}
	for _, reg := range []string{"europe", "america"} {
		rec := request(regions[reg], "GET", url, reg, 2*time.Second)
		if rec.Code != 200 || !strings.HasPrefix(rec.Header().Get("X-Accel-Redirect"), imageFilePrefix+"g2/") {
			t.Fatal("old epoch after reset", reg, rec.Code)
		}
	}
	checkFeed("europe", 1, time.Second)
	db.MustExec("INSERT INTO ip_bans(ip,banned_until) VALUES($1::text,now()+interval '1 hour')", clientIP)
	for _, path := range []string{feed, url} {
		if rec := request(regions["europe"], "GET", path, "europe", time.Second); rec.Code != 403 || !strings.Contains(rec.Body.String(), "ip_banned") {
			t.Fatal("cached IP authorization after ban", rec.Code)
		}
	}
	db.MustExec("DELETE FROM ip_bans")

	db.MustExec("UPDATE posts SET caption='edited' WHERE id='asia'")
	if checkFeed("europe", 1, time.Second).Items[0].Caption != "edited" {
		t.Fatal("old caption")
	}
	for _, reg := range []string{"europe", "america"} {
		checkImage(reg, 2*time.Second)
	}
	db.MustExec("UPDATE posts SET deleted_at=clock_timestamp() WHERE id='asia'")
	for _, reg := range []string{"europe", "america"} {
		if r := request(regions[reg], "GET", url, reg, time.Second); r.Code != 403 {
			t.Fatalf("deleted image in %s: %d", reg, r.Code)
		}
		checkFeed(reg, 0, time.Second)
	}
	db.MustExec(`UPDATE login_sessions SET active=false WHERE user_id='europe'`)
	if r := request(regions["europe"], "GET", feed, "europe", time.Second); r.Code != 401 {
		t.Fatalf("revoked token: %d", r.Code)
	}
	db.MustExec(`UPDATE users SET region='europe' WHERE id='america'`)
	if r := request(regions["america"], "GET", feed, "america", time.Second); r.Code != 403 {
		t.Fatalf("moved user: %d", r.Code)
	}
	// The inverse migration is atomic and repeatable, also after an unused
	// installation. A running binary falls back to authoritative reads.
	remove, err := os.ReadFile("../sql/remove-regional-read-leases.sql")
	if err != nil {
		t.Fatal(err)
	}
	db.MustExec(string(remove))
	db.MustExec(string(remove))
	db.MustExec("UPDATE login_sessions SET active=true WHERE user_id='europe'")
	checkFeed("europe", 0, time.Second)

}
