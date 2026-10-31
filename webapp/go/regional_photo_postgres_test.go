package isureal

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"image"
	"image/jpeg"
	"io"
	"math/rand"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jmoiron/sqlx"
)

func applyPhotoTestSQL(t *testing.T, db *sqlx.DB, name string) {
	t.Helper()
	sql, err := os.ReadFile(filepath.Join("../sql", name))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(string(sql)); err != nil {
		t.Fatalf("%s: %v", name, err)
	}
}

func TestRegionalPhotoLifecycle(t *testing.T) {
	db := isolatedWriteTestDB(t)
	for _, n := range []string{"001-schema.sql", "005-image-cache.sql", "006-notification-batches.sql", "011-regional-post-photos.sql"} {
		applyPhotoTestSQL(t, db, n)
	}
	db.MustExec(`INSERT INTO users(id,username,display_name,secret_key_hash,region) VALUES
		('author','author','Author',repeat('12',32),'europe'),('friend','friend','Friend',repeat('34',32),'america');
		INSERT INTO friendships(id,user_low_id,user_high_id) VALUES('friendship','author','friend');
		INSERT INTO image_objects(id,owner_user_id,data,media_type,created_at) VALUES('historic','author','original','image/jpeg','2025-01-15');
		UPDATE users SET profile_image_id='historic' WHERE id='author'`)
	applyPhotoTestSQL(t, db, "003-initial-snapshot.sql")
	db.MustExec(`UPDATE image_cache_generation SET regional_post_photos=true;
		INSERT INTO posting_windows(id,region,local_date,starts_at,on_time_deadline) VALUES('day','europe',CURRENT_DATE,now()-interval '1 minute',now()+interval '2 minutes'),('us-day','america',CURRENT_DATE,now()-interval '1 minute',now()+interval '2 minutes');
		INSERT INTO posts(id,author_id,posting_window_id,image_id,visibility,location_kind,posted_at) VALUES('friends-post','friend','us-day','friend-image','friends','hidden',now())`)
	for _, id := range []string{"author", "friend"} {
		hash := sha256.Sum256([]byte(id))
		db.MustExec(`INSERT INTO login_sessions(id,user_id,token_hash) VALUES($1,$1,$2)`, id, hex.EncodeToString(hash[:]))
	}
	stores := map[string]*regionalPhotoStore{}
	origins := map[string]string{}
	var deleteDuringFetch atomic.Bool
	start := func(region, root string) *httptest.Server {
		cfg := &Config{Region: region, LogLevel: "error"}
		store := newPhotoStore(cfg)
		store.root = root
		store.origins = origins
		cfg.Photos = store
		stores[region] = store
		app := NewServerWithDB(cfg, db)
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.HasPrefix(r.URL.Path, privatePhotoPath) && deleteDuringFetch.Swap(false) {
				db.MustExec("UPDATE posts SET deleted_at=now() WHERE author_id='author' AND deleted_at IS NULL")
			}
			app.ServeHTTP(w, r)
		}))
	}
	for _, region := range []string{"asia", "europe", "america"} {
		server := start(region, filepath.Join(t.TempDir(), "originals"))
		t.Cleanup(server.Close)
		origins[region] = server.URL
	}
	do := func(region, method, path, token, contentType string, body []byte, want int) []byte {
		t.Helper()
		req, err := http.NewRequest(method, origins[region]+path, bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		if contentType != "" {
			req.Header.Set("Content-Type", contentType)
		}
		response, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		out, err := io.ReadAll(response.Body)
		if err != nil {
			t.Fatal(err)
		}
		if response.StatusCode != want {
			t.Fatalf("%s %s %s got %d want %d: %.300s", region, method, path, response.StatusCode, want, out)
		}
		return out
	}
	photo := image.NewGray(image.Rect(0, 0, 896, 896))
	rand.New(rand.NewSource(42)).Read(photo.Pix)
	var jpegBytes, form bytes.Buffer
	if err := jpeg.Encode(&jpegBytes, photo, nil); err != nil {
		t.Fatal(err)
	}
	mw := multipart.NewWriter(&form)
	for k, v := range map[string]string{"caption": "photo", "visibility": "friends", "location_kind": "exact", "latitude": "35", "longitude": "139"} {
		if err := mw.WriteField(k, v); err != nil {
			t.Fatal(err)
		}
	}
	part, err := mw.CreateFormFile("image", "photo.jpg")
	if err != nil {
		t.Fatal(err)
	}
	part.Write(jpegBytes.Bytes())
	mw.Close()
	post := func() createPostResponse {
		var reply createPostResponse
		raw := do("europe", "POST", "/api/posts", "author", mw.FormDataContentType(), form.Bytes(), 201)
		if err := json.Unmarshal(raw, &reply); err != nil {
			t.Fatal(err)
		}
		return reply
	}
	first := post()
	var pointer []byte
	if err := db.Get(&pointer, `SELECT i.data FROM image_objects i JOIN posts p ON i.id=p.image_id AND i.created_at=p.posted_at WHERE p.id=$1`, first.ID); err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(pointer, []byte(photoMagic+"europe/")) {
		t.Fatalf("expected regional photo, stored length %d", len(pointer))
	}
	t.Logf("JPEG %d bytes, primary DB value %d bytes", jpegBytes.Len(), len(pointer))
	for _, r := range []string{"asia", "america", "europe"} {
		imageBodies.clear()
		if out := do(r, "GET", first.ImageURL, "", "", nil, 200); !bytes.Equal(out, jpegBytes.Bytes()) {
			t.Fatal(r + " different image")
		}
	}
	private := privatePhotoPath + strings.TrimPrefix(string(pointer), photoMagic)
	do("europe", "GET", private, "", "", nil, 403)
	do("europe", "POST", privatePhotoPath+"europe/prune", "", "", nil, 403)
	do("america", "GET", first.ImageURL+"x", "", "", nil, 403)
	do("america", "GET", "/api/posts/"+first.ID, "friend", "", nil, 200)
	do("america", "PUT", "/api/blocks/author", "friend", "", nil, 204)
	do("america", "GET", "/api/posts/"+first.ID, "friend", "", nil, 404)

	// Start a fresh origin against the on-disk files, then move the author so export
	// must fetch the unchanged JPEG from another region using the private route.
	restarted := start("europe", stores["europe"].root)
	defer restarted.Close()
	origins["europe"] = restarted.URL
	imageBodies.clear()
	if out := do("asia", "GET", first.ImageURL, "", "", nil, 200); !bytes.Equal(out, jpegBytes.Bytes()) {
		t.Fatal("restart lost photo")
	}
	db.MustExec("UPDATE users SET region='america' WHERE id='author'")
	raw := do("america", "GET", "/api/me/export", "author", "", nil, 200)
	z, err := zip.NewReader(bytes.NewReader(raw), int64(len(raw)))
	if err != nil {
		t.Fatal(err)
	}
	zf, err := z.Open("images/000001.jpg")
	if err != nil {
		t.Fatal(err)
	}
	extracted, err := io.ReadAll(zf)
	zf.Close()
	if err != nil || !bytes.Equal(extracted, jpegBytes.Bytes()) {
		t.Fatal("export changed JPEG")
	}
	db.MustExec("UPDATE users SET region='europe' WHERE id='author'")

	imageBodies.clear()
	deleteDuringFetch.Store(true)
	do("america", "GET", first.ImageURL, "", "", nil, 403)
	do("europe", "GET", first.ImageURL, "", "", nil, 403)
	second := post()
	if err := stores["europe"].restore(context.Background(), db); err == nil {
		t.Fatal("restore accepted live regional writes")
	}
	db.MustExec("UPDATE image_cache_generation SET regional_post_photos=false")
	if err := stores["europe"].restore(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	if err := stores["europe"].restore(context.Background(), db); err != nil {
		t.Fatal("idempotent restore:", err)
	}
	var restored int
	if err := db.Get(&restored, "SELECT count(*) FROM image_objects WHERE data=$1", jpegBytes.Bytes()); err != nil || restored != 2 {
		t.Fatalf("legacy reader rows %d %v", restored, err)
	}
	var pending int
	if err := db.Get(&pending, "SELECT count(*) FROM regional_post_photos"); err != nil || pending != 0 {
		t.Fatal("undrained rollback", pending, err)
	}
	imageBodies.clear()
	if out := do("america", "GET", second.ImageURL, "", "", nil, 200); !bytes.Equal(out, jpegBytes.Bytes()) {
		t.Fatal("legacy inline bytes changed")
	}
	db.MustExec("UPDATE image_cache_generation SET regional_post_photos=true")
	applyPhotoTestSQL(t, db, "reset.sql")
	if err := stores["asia"].reset(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(stores["europe"].root, "g1")); !os.IsNotExist(err) {
		t.Fatal("old files survived reset", err)
	}
	if _, err := stores["europe"].store(1, jpegBytes.Bytes()); err == nil {
		t.Fatal("obsolete epoch recreated")
	}
	do("america", "GET", second.ImageURL, "", "", nil, 403)
	var historical []byte
	if err := db.Get(&historical, "SELECT data FROM image_objects WHERE id='historic'"); err != nil || string(historical) != "original" {
		t.Fatal("lost historical image", err)
	}
}

func TestRegionalPhotoPrivateSignature(t *testing.T) {
	store := newPhotoStore(&Config{Region: "europe"})
	store.root = t.TempDir()
	store.key = bytes.Repeat([]byte{1}, 32)
	ref, err := store.store(5, []byte("immutable"))
	if err != nil {
		t.Fatal(err)
	}
	path := privatePhotoPath + ref.path()
	for _, tc := range []struct {
		method, path, expires string
		want                  bool
	}{
		{"GET", path, "30", true}, {"POST", path, "30", false}, {"GET", path + "a", "30", false}, {"GET", path, "-1", false}, {"GET", path, "125", false},
	} {
		delta, _ := time.ParseDuration(tc.expires + "s")
		exp := fmt.Sprint(time.Now().Add(delta).Unix())
		req := httptest.NewRequest(tc.method, tc.path, nil)
		req.Header.Set("X-Isureal-Photo-Expires", exp)
		req.Header.Set("X-Isureal-Photo-Mac", hex.EncodeToString(store.signature("GET", path, exp)))
		if store.authorized(req) != tc.want {
			t.Fatalf("method/path/expiry check: %v", tc)
		}
	}
	for _, raw := range []string{"asia/../secret", "asia/01/" + ref.digest, "elsewhere/1/" + ref.digest, "asia/1/" + strings.ToUpper(ref.digest)} {
		if _, ok := parsePhotoRef(raw); ok {
			t.Fatal("unsafe reference", raw)
		}
	}
}
