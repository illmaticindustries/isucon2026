package isureal

import (
	"archive/zip"
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jmoiron/sqlx"
	"github.com/labstack/echo/v5"
)

// The export must contain exactly the database bytes whether each image comes
// from this host's file cache or from PostgreSQL.
func TestExportUsesImageFilesWithDatabaseFallback(t *testing.T) {
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
 id text PRIMARY KEY, username text NOT NULL, secret_key_hash text NOT NULL,
 display_name text NOT NULL, bio text NOT NULL DEFAULT '', profile_image_id text,
 region text NOT NULL DEFAULT 'asia', account_type text NOT NULL DEFAULT 'regular', status text NOT NULL DEFAULT 'active')`)
	db.MustExec(`CREATE TEMP TABLE posting_windows (id text PRIMARY KEY, region text, local_date date, starts_at timestamptz, on_time_deadline timestamptz, ended_at timestamptz)`)
	db.MustExec(`CREATE TEMP TABLE posts (id text PRIMARY KEY,author_id text,posting_window_id text,image_id text,caption text,visibility text,
 location_kind text,latitude numeric,longitude numeric,locality text,posted_at timestamptz,deleted_at timestamptz)`)
	db.MustExec(`CREATE TEMP TABLE image_objects (id text, owner_user_id text, data bytea, media_type text, created_at timestamptz, PRIMARY KEY (id, created_at))`)
	db.MustExec(`CREATE TEMP TABLE image_cache_generation (generation bigint)`)
	db.MustExec(`INSERT INTO image_cache_generation VALUES (7)`)
	// p1 and p2 are initial data; p4 was created by the benchmark.
	db.MustExec(`CREATE TEMP TABLE initial_data_boundary (created_at timestamptz)`)
	db.MustExec(`INSERT INTO initial_data_boundary VALUES ('2026-10-30 12:00:00+00')`)
	db.MustExec(`INSERT INTO users(id,username,secret_key_hash,display_name) VALUES ('author','author',repeat('00',32),'Author'),('other','other',repeat('00',32),'Other')`)
	db.MustExec(`INSERT INTO posting_windows VALUES ('w1','asia','2026-10-30','2026-10-30','2026-10-30 00:10:00+00','2026-10-31'),
 ('w2','asia','2026-10-31','2026-10-31','2026-10-31 00:10:00+00',NULL)`)

	base := time.Date(2026, 10, 30, 0, 5, 0, 123456000, time.UTC)
	posts := []struct {
		id, author, window string
		at                 time.Time
		deleted            bool
	}{
		// p2 and p1 share a timestamp to check the id tiebreak.
		{"p2", "author", "w1", base, false},
		{"p1", "author", "w1", base, false},
		{"p3", "author", "w2", base.Add(24 * time.Hour), true},
		{"p4", "author", "w2", base.Add(24*time.Hour + time.Minute), false},
		{"p5", "other", "w2", base.Add(24*time.Hour + 2*time.Minute), false},
	}
	images := map[string][]byte{}
	for i, p := range posts {
		image := "image-" + p.id
		images[image] = bytes.Repeat([]byte{byte(i + 1), 0xff, 0xd8}, 1000+i)
		var deleted *time.Time
		if p.deleted {
			deleted = &p.at
		}
		db.MustExec(`INSERT INTO posts VALUES ($1,$2,$3,$4,'caption '||$1,'friends','none',NULL,NULL,NULL,$5,$6)`, p.id, p.author, p.window, image, p.at, deleted)
		db.MustExec(`INSERT INTO image_objects VALUES ($1,$2,$3,'image/jpeg',$4)`, image, p.author, images[image], p.at)
	}

	export := func(store *imageFileStore) map[string][]byte {
		t.Helper()
		e := echo.New()
		rec := httptest.NewRecorder()
		c := e.NewContext(httptest.NewRequest(http.MethodGet, "/api/me/export", nil), rec)
		c.Set("session", &Session{UserID: "author"})
		if err := exportPostsHandler(&HandlerContext{DB: db, ImageFiles: store})(c); err != nil {
			t.Fatal(err)
		}
		if rec.Code != http.StatusOK {
			t.Fatalf("status %d", rec.Code)
		}
		zr, err := zip.NewReader(bytes.NewReader(rec.Body.Bytes()), int64(rec.Body.Len()))
		if err != nil {
			t.Fatal(err)
		}
		files := map[string][]byte{}
		for _, f := range zr.File {
			r, err := f.Open()
			if err != nil {
				t.Fatal(err)
			}
			files[f.Name], _ = io.ReadAll(r)
			r.Close()
		}
		return files
	}
	check := func(name string, files map[string][]byte) {
		t.Helper()
		var manifest []struct {
			ID        string `json:"id"`
			ImagePath string `json:"imagePath"`
		}
		if err := json.Unmarshal(files["posts.json"], &manifest); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		want := []string{"p1", "p2", "p4"}
		if len(manifest) != len(want) || len(files) != len(want)+1 {
			t.Fatalf("%s: manifest %+v, %d files", name, manifest, len(files))
		}
		for i, item := range manifest {
			path := fmt.Sprintf("images/%06d.jpg", i+1)
			if item.ID != want[i] || item.ImagePath != path || !bytes.Equal(files[path], images["image-"+want[i]]) {
				t.Fatalf("%s: item %d = %+v, image matches=%t", name, i, item, bytes.Equal(files[path], images["image-"+want[i]]))
			}
		}
	}

	// No file cache, an empty one, and a partly warmed one all match PostgreSQL.
	check("nil store", export(nil))
	store := &imageFileStore{root: filepath.Join(t.TempDir(), "files")}
	check("empty store", export(store))
	boundary := sql.NullTime{Time: time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC), Valid: true}
	initialKey := func(image string) string { return imageFileKey(image, base, "image/jpeg", 7, boundary) }
	store.warmJPEG(insertedImage{CreatedAt: base, Generation: 7}, "image-p4", images["image-p4"])
	store.warmJPEG(insertedImage{CreatedAt: base, Generation: 7}, "image-p1", images["image-p1"])
	fresh := &imageFileStore{root: filepath.Join(t.TempDir(), "fresh")}
	fresh.warmJPEG(insertedImage{CreatedAt: base, Generation: 7}, "image-p1", images["image-p1"])
	if _, ok := fresh.read(initialKey("image-p1")); ok {
		t.Fatal("warmJPEG must not write the initial group")
	}
	check("partial store", export(store))

	// The export stores the images it read from PostgreSQL in the background.
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		_, ok1 := store.read(initialKey("image-p1"))
		_, ok2 := store.read(initialKey("image-p2"))
		if ok1 && ok2 && len(exportWarmer) == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("export did not store the initial images it read")
		}
	}

	// A cached file is served without reading the image from PostgreSQL.
	store.warmJPEG(insertedImage{CreatedAt: base.Add(24*time.Hour + time.Minute), Generation: 7}, "image-p4", images["image-p4"])
	db.MustExec(`UPDATE image_objects SET data = 'not used'`)
	check("warm store", export(store))

	// After initialize, initial images still come from files, while the
	// benchmark image of the old epoch is read from PostgreSQL again.
	db.MustExec(`UPDATE image_cache_generation SET generation = 8`)
	if err := store.reset(8); err != nil {
		t.Fatal(err)
	}
	db.MustExec(`UPDATE image_objects i SET data = $1 WHERE id = 'image-p4'`, images["image-p4"])
	check("after reset", export(store))
	db.MustExec(`UPDATE image_objects i SET data = $1 WHERE id = 'image-p1'`, images["image-p1"])
	db.MustExec(`UPDATE image_objects i SET data = $1 WHERE id = 'image-p2'`, images["image-p2"])

	// Files of an older generation are never used.
	stale := &imageFileStore{root: filepath.Join(t.TempDir(), "stale")}
	stale.warmJPEG(insertedImage{CreatedAt: base.Add(24*time.Hour + time.Minute), Generation: 7}, "image-p4", []byte("stale"))
	check("stale generation", export(stale))
	// Let the background fill finish before the temporary directories go away.
	for deadline := time.Now().Add(5 * time.Second); len(exportWarmer) > 0 && time.Now().Before(deadline); {
		time.Sleep(10 * time.Millisecond)
	}
}
