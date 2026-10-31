package isureal

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/jmoiron/sqlx"
	"github.com/labstack/echo/v5"
)

const photoMagic = "isureal-photo-v1/"
const privatePhotoPath = "/_isureal_origin_photo/"

type regionalPhotoStore struct {
	filesMu      sync.RWMutex
	minEpoch     int64
	modeMu       sync.Mutex
	modeUntil    time.Time
	modeEpoch    int64
	root, region string
	key          []byte
	origins      map[string]string
	client       *http.Client
}

func newPhotoStore(config *Config) *regionalPhotoStore {
	return &regionalPhotoStore{
		root: "/home/isucon/.isureal-post-originals", region: config.Region,
		origins: map[string]string{"asia": "http://192.168.1.10", "europe": "http://192.168.2.10", "america": "http://192.168.3.10"},
		client:  &http.Client{Transport: &http.Transport{MaxIdleConnsPerHost: 16, IdleConnTimeout: 90 * time.Second}, Timeout: 20 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }},
	}
}

// Called before the listener accepts requests; the same physical store is shared
// with its forwarded listeners. They never reload or log the secret.
func (s *regionalPhotoStore) loadKey(ctx context.Context, db *sqlx.DB) error {
	s.modeMu.Lock()
	defer s.modeMu.Unlock()
	if len(s.key) > 0 {
		return nil
	}
	return db.GetContext(ctx, &s.key, "SELECT key_bytes FROM regional_photo_peer_key WHERE singleton")
}

// This hint selects the storage path. The authoritative transaction checks it
// under a shared lock before publishing any reference.
func (s *regionalPhotoStore) writeEpoch(ctx context.Context, db *sqlx.DB) (int64, error) {
	s.modeMu.Lock()
	defer s.modeMu.Unlock()
	if time.Now().Before(s.modeUntil) {
		return s.modeEpoch, nil
	}
	var epoch int64
	if err := db.GetContext(ctx, &epoch, `SELECT CASE WHEN regional_post_photos THEN generation ELSE -1 END FROM image_cache_generation`); err != nil {
		return -1, err
	}
	s.modeUntil = time.Now().Add(time.Second)
	s.modeEpoch = epoch
	return epoch, nil
}

type photoRef struct{ region, epoch, digest string }

func (r photoRef) path() string { return r.region + "/" + r.epoch + "/" + r.digest }
func parsePhotoRef(s string) (photoRef, bool) {
	fields := strings.Split(s, "/")
	if len(fields) != 3 || !validRegion(fields[0]) || len(fields[2]) != sha256.Size*2 {
		return photoRef{}, false
	}
	gen, err := strconv.ParseInt(fields[1], 10, 64)
	if err != nil || gen < 0 || fields[1] != strconv.FormatInt(gen, 10) {
		return photoRef{}, false
	}
	b, err := hex.DecodeString(fields[2])
	if err != nil || hex.EncodeToString(b) != fields[2] {
		return photoRef{}, false
	}
	return photoRef{fields[0], fields[1], fields[2]}, true
}

func syncDir(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}

// Sync the file and both newly created directories before PostgreSQL may commit a reference.
func (s *regionalPhotoStore) store(epoch int64, data []byte) (photoRef, error) {
	s.filesMu.RLock()
	defer s.filesMu.RUnlock()
	if epoch < s.minEpoch {
		return photoRef{}, errors.New("obsolete photo generation")
	}
	digest := sha256.Sum256(data)
	ref := photoRef{s.region, strconv.FormatInt(epoch, 10), hex.EncodeToString(digest[:])}
	dir := filepath.Join(s.root, "g"+ref.epoch)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return ref, err
	}
	var stat syscall.Statfs_t
	if err := syscall.Statfs(dir, &stat); err != nil {
		return ref, err
	}
	if uint64(stat.Bavail)*uint64(stat.Bsize) < (4<<30)+uint64(len(data)) {
		return ref, errors.New("regional photo disk reserve reached")
	}
	f, err := os.CreateTemp(dir, ".pending-")
	if err != nil {
		return ref, err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(data); err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return ref, err
	}
	if closeErr != nil {
		return ref, closeErr
	}
	if err = os.Rename(f.Name(), filepath.Join(dir, ref.digest)); err != nil {
		return ref, err
	}
	for _, path := range []string{dir, s.root, filepath.Dir(s.root)} {
		if err := syncDir(path); err != nil {
			return ref, err
		}
	}
	return ref, nil
}

func (s *regionalPhotoStore) signature(method, path, exp string) []byte {
	h := hmac.New(sha256.New, s.key)
	fmt.Fprintf(h, "%s\n%s\n%s", method, path, exp)
	return h.Sum(nil)
}
func (s *regionalPhotoStore) request(ctx context.Context, method, region, path string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, s.origins[region]+path, nil)
	if err != nil {
		return nil, err
	}
	exp := strconv.FormatInt(time.Now().Add(30*time.Second).Unix(), 10)
	req.Header.Set("X-Isureal-Photo-Expires", exp)
	req.Header.Set("X-Isureal-Photo-Mac", hex.EncodeToString(s.signature(method, path, exp)))
	return s.client.Do(req)
}
func (s *regionalPhotoStore) authorized(req *http.Request) bool {
	exp := req.Header.Get("X-Isureal-Photo-Expires")
	end, err := strconv.ParseInt(exp, 10, 64)
	now := time.Now().Unix()
	sig, decodeErr := hex.DecodeString(req.Header.Get("X-Isureal-Photo-Mac"))
	return len(s.key) == sha256.Size && err == nil && end >= now && end <= now+60 && decodeErr == nil && hmac.Equal(sig, s.signature(req.Method, req.URL.Path, exp))
}
func (s *regionalPhotoStore) serve(c *echo.Context) error {
	if !s.authorized(c.Request()) {
		return c.NoContent(http.StatusForbidden)
	}
	if c.Param("*") == s.region+"/probe" {
		return c.NoContent(http.StatusNoContent)
	}
	r, ok := parsePhotoRef(c.Param("*"))
	if !ok || r.region != s.region {
		return c.NoContent(http.StatusForbidden)
	}
	f, err := os.Open(filepath.Join(s.root, "g"+r.epoch, r.digest))
	if err != nil {
		return err
	}
	defer f.Close()
	c.Response().Header().Set("Cache-Control", "no-store")
	return c.Stream(http.StatusOK, "application/octet-stream", f)
}

func (s *regionalPhotoStore) prune(ctx context.Context, db *sqlx.DB) error {
	var epoch int64
	if err := db.GetContext(ctx, &epoch, "SELECT generation FROM image_cache_generation"); err != nil {
		return err
	}
	s.filesMu.Lock()
	defer s.filesMu.Unlock()
	if epoch < s.minEpoch {
		return nil
	}
	s.minEpoch = epoch
	return (&imageFileStore{root: s.root}).reset(epoch)
}

func (s *regionalPhotoStore) cleanupHandler(db *sqlx.DB) echo.HandlerFunc {
	return func(c *echo.Context) error {
		if c.Param("region") != s.region || !s.authorized(c.Request()) {
			return c.NoContent(http.StatusForbidden)
		}
		// Never accept a generation from a peer as an instruction to delete files.
		if err := s.prune(c.Request().Context(), db); err != nil {
			return err
		}
		return c.NoContent(http.StatusNoContent)
	}
}

func (s *regionalPhotoStore) peers(ctx context.Context, method, operation string) error {
	for region := range s.origins {
		if region == s.region {
			continue
		}
		resp, err := s.request(ctx, method, region, privatePhotoPath+region+"/"+operation)
		if err != nil {
			return err
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusNoContent {
			return fmt.Errorf("regional photo %s on %s: HTTP %d", operation, region, resp.StatusCode)
		}
	}
	return nil
}
func (s *regionalPhotoStore) reset(ctx context.Context, db *sqlx.DB) error {
	if err := s.prune(ctx, db); err != nil {
		return err
	}
	var enabled bool
	if err := db.GetContext(ctx, &enabled, "SELECT regional_post_photos FROM image_cache_generation"); err != nil {
		return err
	}
	if enabled {
		return s.peers(ctx, http.MethodPost, "prune")
	}
	return nil
}

func (s *regionalPhotoStore) resolve(ctx context.Context, data []byte) ([]byte, error) {
	if !bytes.HasPrefix(data, []byte(photoMagic)) {
		return data, nil
	}
	r, ok := parsePhotoRef(string(data[len(photoMagic):]))
	if !ok || s == nil {
		return nil, errors.New("invalid regional photo reference")
	}
	var body []byte
	var err error
	if r.region == s.region {
		body, err = os.ReadFile(filepath.Join(s.root, "g"+r.epoch, r.digest))
	} else {
		path := privatePhotoPath + r.path()
		resp, e := s.request(ctx, http.MethodGet, r.region, path)
		if e != nil {
			return nil, e
		}
		defer resp.Body.Close()
		if resp.StatusCode != 200 {
			return nil, fmt.Errorf("private photo origin returned %d", resp.StatusCode)
		}
		body, err = io.ReadAll(io.LimitReader(resp.Body, (32<<20)+1))
	}
	if err != nil {
		return nil, err
	}
	hash := sha256.Sum256(body)
	if hex.EncodeToString(hash[:]) != r.digest {
		return nil, errors.New("regional photo digest mismatch")
	}
	return body, nil
}

// Operational commands run only from the reserved deploy/restore target.
func RegionalPhotoCommand(ctx context.Context, db *sqlx.DB, config *Config, command string) error {
	store := config.Photos
	if err := store.loadKey(ctx, db); err != nil {
		return err
	}
	switch command {
	case "check-regional-photos":
		// Exercise the actual filesystem and inter-region routes before retiring inline writes.
		var generation int64
		if err := db.GetContext(ctx, &generation, "SELECT generation FROM image_cache_generation"); err != nil {
			return err
		}
		if _, err := store.store(generation, []byte("regional photo storage readiness probe")); err != nil {
			return err
		}
		return store.peers(ctx, http.MethodGet, "probe")
	case "restore-regional-photos":
		return store.restore(ctx, db)
	default:
		return errors.New("unknown regional photo command")
	}
}

// Restore one durable image per transaction. Retrying after interruption is safe.
func (s *regionalPhotoStore) restore(ctx context.Context, db *sqlx.DB) error {
	var enabled bool
	if err := db.GetContext(ctx, &enabled, "SELECT regional_post_photos FROM image_cache_generation"); err != nil {
		return err
	}
	if enabled {
		return errors.New("disable regional photo writes before restoring")
	}
	for {
		var rows []struct {
			ID   string    `db:"image_id"`
			At   time.Time `db:"created_at"`
			Data []byte    `db:"data"`
		}
		if err := db.SelectContext(ctx, &rows, `SELECT r.image_id,r.created_at,i.data FROM regional_post_photos r JOIN image_objects i ON i.id=r.image_id AND i.created_at=r.created_at WHERE r.region=$1 LIMIT 32`, s.region); err != nil {
			return err
		}
		if len(rows) == 0 {
			return nil
		}
		for _, row := range rows {
			data, err := s.resolve(ctx, row.Data)
			if err != nil {
				return err
			}
			if _, err = db.ExecContext(ctx, `WITH restored AS (UPDATE image_objects SET data=$3 WHERE id=$1 AND created_at=$2 AND data=$4 RETURNING id,created_at)
				DELETE FROM regional_post_photos r USING restored WHERE r.image_id=restored.id AND r.created_at=restored.created_at`, row.ID, row.At, data, row.Data); err != nil {
				return err
			}
		}
	}
}
