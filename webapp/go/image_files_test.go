package isureal

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jmoiron/sqlx"
	"github.com/labstack/echo/v5"
)

func TestImageFileEpoch(t *testing.T) {
	s := &imageFileStore{root: t.TempDir()}
	at := time.Now()
	key := fileImageKey("../../unsafe", at, "image/jpeg", 10)
	s.warmJPEG(insertedImage{at, 10}, "../../unsafe", []byte("committed"))
	data, err := os.ReadFile(filepath.Join(s.root, key))
	if err != nil || string(data) != "committed" || strings.Contains(key, "..") {
		t.Fatal("new upload was not safely materialized", err)
	}
	s = &imageFileStore{root: s.root}
	if !s.exists(key) {
		t.Fatal("restart lost a valid file")
	}
	if err := s.reset(11); err != nil {
		t.Fatal(err)
	}
	if s.exists(key) || s.put(key, 10, []byte("delayed")) {
		t.Fatal("old epoch survived initialization")
	}
	if k := fileImageKey("id", at, "text/html", 11); k != "" {
		t.Fatal("unexpected file type accepted")
	}
}

// Use the actual three nginx site configurations and the actual Go handler.
// Replace only listen/upstream addresses, filesystem paths and the peer ACL
// with loopback test fixtures. PostgreSQL tables are all connection-local TEMP.
func TestRegionalImageNginx(t *testing.T) {
	dsn := os.Getenv("ISUREAL_TEST_DB_DSN")
	if dsn == "" {
		t.Skip("requires test PostgreSQL and nginx")
	}
	nginx, err := exec.LookPath("nginx")
	if err != nil {
		t.Fatal("nginx required for PostgreSQL integration CI")
	}
	db, err := sqlx.Connect("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	db.MustExec(`CREATE TEMP TABLE image_objects(id text, owner_user_id text, data bytea, media_type text, created_at timestamptz);
 CREATE TEMP TABLE image_cache_generation(generation bigint); INSERT INTO image_cache_generation VALUES(321);
 CREATE TEMP TABLE users(id text,status text,secret_key_hash text,profile_image_id text);
 CREATE TEMP TABLE posts(id text,image_id text,author_id text,deleted_at timestamptz);
 CREATE TEMP TABLE realmojis(image_id text,post_id text,reactor_id text,replaced_at timestamptz,deleted_at timestamptz)`)
	dir := t.TempDir()
	s := &imageFileStore{root: filepath.Join(dir, "files")}
	key := bytes.Repeat([]byte{17}, 32)
	db.MustExec(`INSERT INTO users VALUES ('owner','active',$1,'test-id')`, hex.EncodeToString(key))
	tx := db.MustBegin()
	body := []byte{0, 255, 216, 1, 2, 255, 217}
	written, err := insertJPEG(context.Background(), tx, "test-id", "owner", body, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	s.warmJPEG(written, "test-id", body)
	file := fileImageKey("test-id", written.CreatedAt, "image/jpeg", 321)
	var calls [3]atomic.Int32
	var accels, fallbacks atomic.Int32
	var loseFile atomic.Bool
	var addrs, regionalAddrs []string
	for region := range 3 {
		e := echo.New()
		e.HTTPErrorHandler = APIErrorHandler
		e.GET("/api/images/:imageId", getImageHandler(&HandlerContext{DB: db, ImageFiles: s}))
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("X-Test-Backend", fmt.Sprintf("local-%d", region))
			calls[region].Add(1)
			recorder := httptest.NewRecorder()
			e.ServeHTTP(recorder, r)
			if recorder.Header().Get("X-Accel-Redirect") != "" {
				accels.Add(1)
				if loseFile.Swap(false) {
					if err := os.Remove(filepath.Join(s.root, file)); err != nil {
						t.Error(err)
					}
				}
			} else if recorder.Code == 200 {
				fallbacks.Add(1)
			}
			for k, v := range recorder.Header() {
				w.Header()[k] = v
			}
			w.WriteHeader(recorder.Code)
			w.Write(recorder.Body.Bytes())
		}))
		defer server.Close()
		regional := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("X-Test-Backend", fmt.Sprintf("regional-%d", region))
			wantMethod := http.MethodPost
			if r.URL.Path == "/api/users" || strings.HasPrefix(r.URL.Path, "/api/feeds/") {
				wantMethod = http.MethodGet
			}
			if r.URL.Path == "/api/users" && r.URL.Query().Get("q") != "coffee" {
				http.Error(w, "query lost", 400)
				return
			}
			if r.URL.Path == "/api/feeds/friends-of-friends" && r.URL.RawQuery != "beforeScore=7&beforePostedAt=2026-10-31T06%3A00%3A00Z&beforeId=id" {
				http.Error(w, "feed cursor lost", 400)
				return
			}
			if r.Method != wantMethod || r.Header.Get("Authorization") != "Bearer test" {
				http.Error(w, "unexpected request", 400)
			}
		}))
		defer regional.Close()
		regionalAddrs = append(regionalAddrs, strings.TrimPrefix(regional.URL, "http://"))
		addrs = append(addrs, strings.TrimPrefix(server.URL, "http://"))
	}
	var listeners []net.Listener
	var ports []string
	for range 5 {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		listeners = append(listeners, l)
		ports = append(ports, strconv.Itoa(l.Addr().(*net.TCPAddr).Port))
	}
	var sites strings.Builder
	for i, name := range []string{"isureal.conf", "isureal-europe.conf", "isureal-america.conf"} {
		b, err := os.ReadFile("../../etc/nginx/sites-available/" + name)
		if err != nil {
			t.Fatal(err)
		}
		sites.WriteString(strings.NewReplacer("listen 80 default_server", "listen 127.0.0.1:"+ports[i]+" default_server", "192.168.1.10:8091", "127.0.0.1:"+ports[3], "192.168.1.10:8092", "127.0.0.1:"+ports[4],
			"8091 isureal", ports[3]+" isureal", "8092 isureal", ports[4]+" isureal",
			"127.0.0.1:8080", addrs[i], "192.168.1.10:8081", regionalAddrs[1], "192.168.1.10:8082", regionalAddrs[2],
			// Production uses separate nginx instances; this fixture combines all three sites.
			"isureal_backend", fmt.Sprintf("local_backend_%d", i),
			"isureal_regional_backend", fmt.Sprintf("relationship_backend_%d", i),
			"allow 192.168.2.10;", "allow 127.0.0.1;", "/home/isucon/.isureal-image-files", s.root).Replace(string(b)))
	}
	conf := "worker_processes 1; master_process off; daemon off; pid " + dir + "/pid; error_log stderr warn; events {} http {access_log off; sendfile on; types { image/jpeg jpg; image/png png; } "
	for _, kind := range []string{"client_body", "proxy", "fastcgi", "uwsgi", "scgi"} {
		conf += kind + "_temp_path " + dir + "/" + kind + "; "
	}
	conf += sites.String() + "}"
	path := filepath.Join(dir, "nginx.conf")
	if err := os.WriteFile(path, []byte(conf), 0600); err != nil {
		t.Fatal(err)
	}
	for _, l := range listeners {
		l.Close()
	}
	cmd := exec.Command(nginx, "-p", dir, "-c", path)
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { cmd.Process.Kill(); cmd.Wait() }()
	client := &http.Client{Timeout: 3 * time.Second}
	for i := 0; ; i++ {
		conn, err := net.DialTimeout("tcp", "127.0.0.1:"+ports[0], 50*time.Millisecond)
		if err == nil {
			conn.Close()
			break
		}
		if i == 100 {
			t.Fatal("nginx did not start")
		}
		time.Sleep(20 * time.Millisecond)
	}
	exp := time.Now().Add(5 * time.Minute).Unix()
	signed := func(exp int64) string {
		return fmt.Sprintf("/api/images/test-id?exp=%d&sig=%s", exp, base64.RawURLEncoding.EncodeToString(imageSignature(key, "test-id", exp)))
	}
	get := func(region int, path string, headers map[string]string, status int) {
		t.Helper()
		req, _ := http.NewRequest("GET", "http://127.0.0.1:"+ports[region]+path, nil)
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode != status {
			t.Fatalf("region %d: want %d got %d", region, status, resp.StatusCode)
		}
		if status == 200 && (!bytes.Equal(b, body) || resp.Header.Get("Content-Type") != "image/jpeg" || resp.Header.Get("Cache-Control") != "no-store" || resp.Header.Get("ETag") != "") {
			t.Errorf("region %d body/headers changed: %v", region, resp.Header)
		}
	}
	for region := range 3 {
		get(region, signed(exp), nil, 200)
		if calls[region].Load() != 1 || accels.Load() != int32(region+1) {
			t.Fatalf("incorrect region or first-read file offload: %d", region)
		}
		get(region, imageFilePrefix+file, nil, 404)
	}
	get(1, signed(exp), map[string]string{"Range": "bytes=0-1"}, 200)
	get(2, signed(exp), map[string]string{"If-None-Match": "\"abc\""}, 200)
	if fallbacks.Load() != 2 {
		t.Fatal("unexpected conditional file offload")
	}
	loseFile.Store(true)
	get(1, signed(exp), nil, 200)
	if fallbacks.Load() != 3 {
		t.Fatal("missing file did not revalidate and fetch bytes from Go")
	}
	get(2, signed(exp), nil, 200) // cold fill after removal
	get(1, signed(exp)+"x", nil, 403)
	get(2, signed(time.Now().Unix()-1), nil, 403)
	db.MustExec("UPDATE users SET profile_image_id=NULL")
	for region := range 3 {
		get(region, signed(exp), nil, 403)
	}
	for _, region := range []int{1, 2} {
		for _, path := range []string{"/api/friend-requests", "/api/friend-requests/id/accept", "/api/friend-requests/id/reject", "/api/friend-requests/id/cancel", "/api/friend-requests-other", "/api/relationships/friends", "/api/posts", "/api/users?q=coffee", "/api/posts/id", "/api/users/id",
			"/api/feeds/friends", "/api/feeds/public", "/api/feeds/following",
			"/api/feeds/friends-of-friends?beforeScore=7&beforePostedAt=2026-10-31T06%3A00%3A00Z&beforeId=id", "/api/feeds/friends-extra"} {
			method := http.MethodPost
			if strings.HasPrefix(path, "/api/users") || strings.HasPrefix(path, "/api/feeds/") {
				method = http.MethodGet
			}
			req, err := http.NewRequest(method, "http://127.0.0.1:"+ports[region]+path, nil)
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Authorization", "Bearer test")
			resp, err := client.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			want := fmt.Sprintf("local-%d", region)
			isFeed := strings.HasPrefix(path, "/api/feeds/") && path != "/api/feeds/friends-extra"
			if isFeed || path == "/api/posts" || path == "/api/users?q=coffee" || path == "/api/friend-requests" || strings.HasPrefix(path, "/api/friend-requests/id/") {
				want = fmt.Sprintf("regional-%d", region)
				if resp.StatusCode != 200 {
					t.Errorf("%s: status %d", path, resp.StatusCode)
				}
			}
			if got := resp.Header.Get("X-Test-Backend"); got != want {
				t.Errorf("%s: got %q want %q", path, got, want)
			}
		}
	}
}
