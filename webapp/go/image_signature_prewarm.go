package isureal

import (
	"context"
	"crypto/sha256"
	"encoding/gob"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jmoiron/sqlx"
	"github.com/labstack/echo/v5"
)

// Memories and export sign every post and RealMoji image of a user, and each
// benchmark touches different users, so the on-demand cache rarely hits and the
// 1000-round HMAC dominated asia1 CPU. Signatures depend only on (owner key,
// image, expiry bucket), so they are computed ahead for existing images while
// the server is idle and persisted so a restart does not start cold.

const (
	prewarmIdleAfter = 3 * time.Second
	prewarmChunk     = 512
	prewarmMaxItems  = 4_000_000
)

var lastRequestAt atomic.Int64

// ActivityMiddleware records request arrival so prewarming yields to traffic.
func ActivityMiddleware(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c *echo.Context) error {
		lastRequestAt.Store(time.Now().UnixNano())
		return next(c)
	}
}

func serverIdle() bool {
	return time.Now().UnixNano()-lastRequestAt.Load() > int64(prewarmIdleAfter)
}

var prewarmed = struct {
	sync.RWMutex
	buckets  map[int64]map[signatureCacheKey][sha256.Size]byte
	complete map[int64]bool
}{buckets: map[int64]map[signatureCacheKey][sha256.Size]byte{}, complete: map[int64]bool{}}

func loadPrewarmedSignature(cacheKey signatureCacheKey) ([sha256.Size]byte, bool) {
	prewarmed.RLock()
	defer prewarmed.RUnlock()
	value, ok := prewarmed.buckets[cacheKey.exp][cacheKey]
	return value, ok
}

func storePrewarmedSignatures(exp int64, keys []signatureCacheKey, values [][sha256.Size]byte) {
	prewarmed.Lock()
	defer prewarmed.Unlock()
	bucket := prewarmed.buckets[exp]
	if bucket == nil {
		bucket = make(map[signatureCacheKey][sha256.Size]byte, len(keys))
		prewarmed.buckets[exp] = bucket
	}
	for i, key := range keys {
		bucket[key] = values[i]
	}
}

func prewarmBucketComplete(exp int64) bool {
	prewarmed.RLock()
	defer prewarmed.RUnlock()
	return prewarmed.complete[exp]
}

func markPrewarmBucketComplete(exp int64) {
	prewarmed.Lock()
	defer prewarmed.Unlock()
	prewarmed.complete[exp] = true
}

func dropPrewarmedBefore(exp int64) {
	prewarmed.Lock()
	defer prewarmed.Unlock()
	for e := range prewarmed.buckets {
		if e < exp {
			delete(prewarmed.buckets, e)
			delete(prewarmed.complete, e)
		}
	}
}

type prewarmItem struct {
	key     string
	imageID string
}

func loadPrewarmItems(ctx context.Context, db *sqlx.DB) ([]prewarmItem, error) {
	// Posts first: they are signed by Memories and export, then active RealMojis.
	rows, err := db.QueryContext(ctx, `
   SELECT u.secret_key_hash, p.image_id FROM posts p JOIN users u ON u.id = p.author_id WHERE p.deleted_at IS NULL
   UNION ALL
   SELECT u.secret_key_hash, r.image_id FROM realmojis r JOIN users u ON u.id = r.reactor_id WHERE r.deleted_at IS NULL AND r.replaced_at IS NULL
   UNION ALL
   SELECT secret_key_hash, profile_image_id FROM users WHERE profile_image_id IS NOT NULL
  `)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	keys := map[string]string{}
	var items []prewarmItem
	for rows.Next() {
		var hash, imageID string
		if err := rows.Scan(&hash, &imageID); err != nil {
			return nil, err
		}
		key, ok := keys[hash]
		if !ok {
			raw, err := imageSigningKey(hash)
			if err != nil {
				continue
			}
			key = string(raw)
			keys[hash] = key
		}
		items = append(items, prewarmItem{key: key, imageID: imageID})
		if len(items) >= prewarmMaxItems {
			break
		}
	}
	return items, rows.Err()
}

// computePrewarmBucket signs items for exp using all cores, but only while idle()
// holds; it waits between chunks otherwise. It returns false if ctx ends first.
func computePrewarmBucket(ctx context.Context, items []prewarmItem, exp int64, idle func() bool) bool {
	var next atomic.Int64
	var wg sync.WaitGroup
	for range runtime.GOMAXPROCS(0) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			keys := make([]signatureCacheKey, 0, prewarmChunk)
			values := make([][sha256.Size]byte, 0, prewarmChunk)
			for {
				for !idle() {
					select {
					case <-ctx.Done():
						return
					case <-time.After(500 * time.Millisecond):
					}
				}
				if ctx.Err() != nil {
					return
				}
				start := int(next.Add(prewarmChunk)) - prewarmChunk
				if start >= len(items) {
					return
				}
				keys, values = keys[:0], values[:0]
				for _, item := range items[start:min(start+prewarmChunk, len(items))] {
					cacheKey := signatureCacheKey{key: item.key, imageID: item.imageID, exp: exp}
					var value [sha256.Size]byte
					copy(value[:], computeImageSignature([]byte(item.key), item.imageID, exp))
					keys = append(keys, cacheKey)
					values = append(values, value)
				}
				storePrewarmedSignatures(exp, keys, values)
			}
		}()
	}
	wg.Wait()
	return ctx.Err() == nil
}

type prewarmFile struct {
	Exp     int64
	Keys    []string
	Entries []prewarmFileEntry
}

type prewarmFileEntry struct {
	Key       uint32
	ImageID   string
	Signature [sha256.Size]byte
}

func savePrewarmBucket(dir string, exp int64) error {
	prewarmed.RLock()
	file := prewarmFile{Exp: exp, Entries: make([]prewarmFileEntry, 0, len(prewarmed.buckets[exp]))}
	keyIndex := map[string]uint32{}
	for cacheKey, value := range prewarmed.buckets[exp] {
		index, ok := keyIndex[cacheKey.key]
		if !ok {
			index = uint32(len(file.Keys))
			keyIndex[cacheKey.key] = index
			file.Keys = append(file.Keys, cacheKey.key)
		}
		file.Entries = append(file.Entries, prewarmFileEntry{Key: index, ImageID: cacheKey.imageID, Signature: value})
	}
	prewarmed.RUnlock()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, "signatures-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := gob.NewEncoder(tmp).Encode(&file); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), filepath.Join(dir, fmt.Sprintf("signatures-%d.gob", exp)))
}

func loadPrewarmFile(path string) (int, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	var file prewarmFile
	if err := gob.NewDecoder(f).Decode(&file); err != nil {
		return 0, err
	}
	keys := make([]signatureCacheKey, 0, len(file.Entries))
	values := make([][sha256.Size]byte, 0, len(file.Entries))
	for _, entry := range file.Entries {
		if int(entry.Key) >= len(file.Keys) {
			return 0, fmt.Errorf("corrupt signature file %s", path)
		}
		keys = append(keys, signatureCacheKey{key: file.Keys[entry.Key], imageID: entry.ImageID, exp: file.Exp})
		values = append(values, entry.Signature)
	}
	storePrewarmedSignatures(file.Exp, keys, values)
	markPrewarmBucketComplete(file.Exp)
	return len(keys), nil
}

// currentPrewarmFiles removes files for expired buckets and returns the rest.
func currentPrewarmFiles(dir string, current int64) map[string]int64 {
	files := map[string]int64{}
	paths, _ := filepath.Glob(filepath.Join(dir, "signatures-*.gob"))
	for _, path := range paths {
		exp, err := strconv.ParseInt(strings.TrimSuffix(strings.TrimPrefix(filepath.Base(path), "signatures-"), ".gob"), 10, 64)
		if err != nil || exp < current {
			_ = os.Remove(path)
			continue
		}
		files[path] = exp
	}
	return files
}

// loadPrewarmFiles restores buckets that are still current.
func loadPrewarmFiles(dir string, current int64) {
	for path, exp := range currentPrewarmFiles(dir, current) {
		started := time.Now()
		n, err := loadPrewarmFile(path)
		if err != nil {
			log.Printf("signature prewarm: load %s: %v", path, err)
			_ = os.Remove(path)
			continue
		}
		log.Printf("signature prewarm: loaded %d signatures for exp %d in %s", n, exp, time.Since(started))
	}
}

func signaturePrewarmDir() string {
	if dir, err := os.UserCacheDir(); err == nil {
		return filepath.Join(dir, "isureal-signatures")
	}
	return filepath.Join(os.TempDir(), "isureal-signatures")
}

// StartSignaturePrewarm runs until ctx ends. Start it once, in the process that
// serves all regions.
func StartSignaturePrewarm(ctx context.Context, db *sqlx.DB) {
	go func() {
		dir := signaturePrewarmDir()
		loadPrewarmFiles(dir, imageSignatureExpiry(time.Now()))
		for {
			now := time.Now()
			current := imageSignatureExpiry(now)
			dropPrewarmedBefore(current)
			currentPrewarmFiles(dir, current)
			targets := []int64{current}
			// Prepare the next bucket during the second half of the current one.
			if next := imageSignatureExpiry(now.Add(signatureCacheBucketWidth / 2)); next != current {
				targets = append(targets, next)
			}
			var items []prewarmItem
			for _, exp := range targets {
				if prewarmBucketComplete(exp) || !serverIdle() {
					continue
				}
				started := time.Now()
				if items == nil {
					var err error
					if items, err = loadPrewarmItems(ctx, db); err != nil {
						log.Printf("signature prewarm: load items: %v", err)
						break
					}
				}
				if !computePrewarmBucket(ctx, items, exp, serverIdle) {
					return
				}
				markPrewarmBucketComplete(exp)
				log.Printf("signature prewarm: computed %d signatures for exp %d in %s", len(items), exp, time.Since(started))
				if err := savePrewarmBucket(dir, exp); err != nil {
					log.Printf("signature prewarm: save: %v", err)
				}
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(15 * time.Second):
			}
		}
	}()
}
