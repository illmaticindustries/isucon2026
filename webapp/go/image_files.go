package isureal

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/jmoiron/sqlx"
)

const imageFilePrefix = "/_isureal_image_files/"

// The authoritative bytes stay in PostgreSQL. Files are immutable derivatives;
// every file belongs to a DB epoch, including initial images.
// Only the physical Asia backend writes files. All public image origins use its
// nginx, including the private region-specific listeners for Europe and America.
type imageFileStore struct {
	root     string
	mu       sync.RWMutex
	minEpoch int64
}

func fileImageKey(id string, created time.Time, mediaType string, epoch int64) string {
	return fileImageKeyInGroup(id, created, mediaType, "g"+strconv.FormatInt(epoch, 10))
}

// imageFileKey keeps initial-data images out of the epoch groups. Initialize
// never changes or deletes an image created at or before the boundary, so the
// file stays identical to PostgreSQL across resets and later epochs reuse it.
// The boundary is part of the group: reloading the dataset starts a new group.
// Validity and signatures are still checked in PostgreSQL on every request.
func imageFileKey(id string, created time.Time, mediaType string, epoch int64, initialBoundary sql.NullTime) string {
	if initialBoundary.Valid && !created.After(initialBoundary.Time) {
		return fileImageKeyInGroup(id, created, mediaType, "i"+strconv.FormatInt(initialBoundary.Time.UnixNano(), 10))
	}
	return fileImageKey(id, created, mediaType, epoch)
}

func fileImageKeyInGroup(id string, created time.Time, mediaType, group string) string {
	var ext string
	switch mediaType {
	case "image/jpeg":
		ext = ".jpg"
	case "image/png":
		ext = ".png"
	default:
		return ""
	}
	digest := sha256.Sum256([]byte(fmt.Sprintf("%s\x00%d", id, created.UnixNano())))
	name := hex.EncodeToString(digest[:])
	return group + "/" + name[:2] + "/" + name + ext
}

func (s *imageFileStore) exists(key string) bool {
	if s == nil || key == "" {
		return false
	}
	info, err := os.Stat(filepath.Join(s.root, key))
	return err == nil && info.Mode().IsRegular()
}

// read returns a cached file. Files are published by atomic rename, so a hit
// always has the complete bytes; any error is a miss and the caller uses the DB.
func (s *imageFileStore) read(key string) ([]byte, bool) {
	if s == nil || key == "" {
		return nil, false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	body, err := os.ReadFile(filepath.Join(s.root, key))
	return body, err == nil
}

func (s *imageFileStore) put(key string, epoch int64, body []byte) bool {
	if s == nil || key == "" {
		return false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if epoch < s.minEpoch {
		return false
	}
	if s.exists(key) {
		return true
	}
	dest := filepath.Join(s.root, key)
	dir := filepath.Dir(dest)
	if os.MkdirAll(dir, 0755) != nil {
		return false
	}
	// These derivatives must never exhaust space needed by the application.
	var disk syscall.Statfs_t
	if syscall.Statfs(dir, &disk) != nil || uint64(disk.Bavail)*uint64(disk.Bsize) < (4<<30)+uint64(len(body)) {
		return false
	}
	f, err := os.CreateTemp(dir, ".incomplete-")
	if err != nil {
		return false
	}
	name := f.Name()
	defer os.Remove(name)
	if _, err = f.Write(body); err == nil {
		err = f.Chmod(0644)
	}
	// Never publish a cache file whose contents could be torn after a crash.
	if err == nil {
		err = f.Sync()
	}
	// A lost directory entry is harmless: PostgreSQL has the original.
	closeErr := f.Close()
	if err != nil || closeErr != nil {
		return false
	}
	return os.Rename(name, dest) == nil
}

// Only directories created for previous application epochs are removed; the
// initial-data groups ("i...") are kept because reset never changes them. A failed
// or interrupted fill cannot expose a partial file: writes use atomic rename.
func (s *imageFileStore) reset(epoch int64) error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if epoch < s.minEpoch {
		return nil
	}
	s.minEpoch = epoch
	dirs, err := os.ReadDir(s.root)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, d := range dirs {
		if !d.IsDir() || !strings.HasPrefix(d.Name(), "g") {
			continue
		}
		old, e := strconv.ParseInt(d.Name()[1:], 10, 64)
		if e == nil && old >= 0 && old < epoch && d.Name() == "g"+strconv.FormatInt(old, 10) {
			if err = os.RemoveAll(filepath.Join(s.root, d.Name())); err != nil {
				return err
			}
		}
	}
	return nil
}

type exportImageFile struct {
	key  string
	data []byte
}

// exportWarmer allows one background fill at a time; a busy fill drops the
// next batch instead of queueing its bytes in memory.
var exportWarmer = make(chan struct{}, 1)

// warmExport stores images an export had to read from PostgreSQL, so the next
// export or image request can use the files. It never delays the response.
func (s *imageFileStore) warmExport(epoch int64, files []exportImageFile) {
	if s == nil || len(files) == 0 {
		return
	}
	select {
	case exportWarmer <- struct{}{}:
	default:
		return
	}
	go func() {
		defer func() { <-exportWarmer }()
		for _, f := range files {
			if !s.put(f.key, epoch, f.data) {
				return
			}
		}
	}()
}

// For conditional and range requests, retain the application's existing HTTP
// behavior. nginx's static-file filters must not introduce a new 206/304/412.
func canAccelImage(r *http.Request) bool {
	if r.Method != http.MethodGet || r.Header.Get("X-Isureal-Image-Accel") != "v1" {
		return false
	}
	for _, name := range []string{"Range", "If-Range", "If-Match", "If-None-Match", "If-Modified-Since", "If-Unmodified-Since"} {
		if _, present := r.Header[http.CanonicalHeaderKey(name)]; present {
			return false
		}
	}
	return true
}

// Return the generation and exact timestamp used by PostgreSQL in the INSERT
// round trip. Populate the derivative only after the surrounding write commits.
type insertedImage struct {
	CreatedAt  time.Time `db:"created_at"`
	Generation int64     `db:"generation"`
}

func insertJPEG(ctx context.Context, tx *sqlx.Tx, id, owner string, data []byte, at *time.Time) (insertedImage, error) {
	var img insertedImage
	err := tx.GetContext(ctx, &img, `INSERT INTO image_objects(id,owner_user_id,data,media_type,created_at)
 VALUES ($1,$2,$3,'image/jpeg',COALESCE($4::timestamptz,clock_timestamp()))
 RETURNING created_at,(SELECT generation FROM image_cache_generation) AS generation`, id, owner, data, at)
	return img, err
}
func (s *imageFileStore) warmJPEG(img insertedImage, id string, data []byte) {
	if s != nil {
		s.put(fileImageKey(id, img.CreatedAt, "image/jpeg", img.Generation), img.Generation, data)
	}
}
