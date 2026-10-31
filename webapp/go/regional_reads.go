package isureal

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/labstack/echo/v5"
)

// The database holds each lease longer than the client does. Start the local
// monotonic clock BEFORE the round trip. A slow fill can never resurrect data.
const (
	friendReadLifetime = 850 * time.Millisecond  // database grant: 950ms
	imageReadLifetime  = 1800 * time.Millisecond // database grant: 2s
)

type regionalReadKey struct {
	uri, auth [32]byte
}

type regionalRead struct {
	key             regionalReadKey
	ip              [32]byte
	until           time.Time
	session         *Session
	feed            *getFeedResponse
	file, mediaType string
	exp             int64
}

// Only feed pages (at most 50 items) and nginx file references are retained.
// Cache bounds are separate so an image burst cannot evict the feed working set.
type regionalReads struct {
	mu            sync.Mutex
	feeds, images map[regionalReadKey]regionalRead
	allowedIPs    map[[32]byte]time.Time
	loading       map[regionalReadKey]bool
}

var regionalReadCounts struct {
	feedHits, feedMisses, imageHits, imageMisses, imageFills atomic.Uint64
}

func (rc *regionalReads) load(key regionalReadKey, ip [32]byte, feed bool) (regionalRead, bool) {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	entries := rc.images
	if feed {
		entries = rc.feeds
	}
	if !time.Now().Before(rc.allowedIPs[ip]) {
		return regionalRead{}, false
	}
	v, ok := entries[key]
	if ok && !time.Now().Before(v.until) {
		delete(entries, key)
		ok = false
	}
	return v, ok
}

func (rc *regionalReads) store(v regionalRead) {
	if !time.Now().Before(v.until) {
		return
	}
	rc.mu.Lock()
	defer rc.mu.Unlock()
	entries, limit := &rc.images, 4096
	if v.feed != nil {
		entries, limit = &rc.feeds, 256
	}
	if *entries == nil || len(*entries) >= limit {
		*entries = make(map[regionalReadKey]regionalRead)
	}
	// An image URL is a bearer capability. A different client may reuse it
	// only while its own non-banned IP also has a live grant.
	if rc.allowedIPs == nil || len(rc.allowedIPs) >= 4096 {
		rc.allowedIPs = make(map[[32]byte]time.Time)
	}
	if v.until.After(rc.allowedIPs[v.ip]) {
		rc.allowedIPs[v.ip] = v.until
	}
	(*entries)[v.key] = v
}

// Deduplicate fills and bound the work that can compete with foreground SQL.
func (rc *regionalReads) startImageFill(key regionalReadKey) bool {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	if rc.loading[key] || len(rc.loading) >= 4 {
		return false
	}
	if rc.loading == nil {
		rc.loading = make(map[regionalReadKey]bool)
	}
	rc.loading[key] = true
	return true
}

func (rc *regionalReads) finishImageFill(key regionalReadKey) {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	delete(rc.loading, key)
}

func (rc *regionalReads) clear() {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	rc.feeds, rc.images, rc.allowedIPs = nil, nil, nil
}

func (rc *regionalReads) ipAllowed(ip [32]byte) bool {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	return time.Now().Before(rc.allowedIPs[ip])
}

// A successful grant checks the real client IP and, for a feed, the active
// token and region under the same lease. Failures use the normal primary path.
func regionalReadMiddleware(h *HandlerContext) echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c *echo.Context) error {
			r := c.Request()
			feed := r.URL.Path == "/api/feeds/friends" && c.Param("feed") == "friends"
			image := c.RouteInfo().Path == "/api/images/:imageId" && canAccelImage(r) && h.ImageFiles != nil
			if r.Method != http.MethodGet || r.ContentLength != 0 || (!feed && !image) {
				return next(c)
			}
			auth := r.Header.Get("Authorization")
			if feed && !strings.HasPrefix(auth, "Bearer ") {
				return next(c)
			}
			ip := c.RealIP()
			ipHash := sha256.Sum256([]byte(ip))
			key := regionalReadKey{uri: sha256.Sum256([]byte(r.URL.RequestURI()))}
			if feed {
				key.auth = sha256.Sum256([]byte(strings.TrimPrefix(auth, "Bearer ")))
			}
			if v, ok := h.regional.load(key, ipHash, feed); ok {
				if feed {
					c.Set("session", v.session)
					regionalReadCounts.feedHits.Add(1)
					return c.JSON(http.StatusOK, v.feed)
				}
				if h.ImageFiles.exists(v.file) && v.exp > time.Now().Unix() {
					regionalReadCounts.imageHits.Add(1)
					c.Response().Header().Set("Cache-Control", "no-store")
					c.Response().Header().Set("Content-Type", v.mediaType)
					c.Response().Header().Set("X-Accel-Redirect", imageFilePrefix+v.file)
					return c.NoContent(http.StatusOK)
				}
			}
			v := &regionalRead{key: key, ip: ipHash}
			c.Set("regional_read", v)
			if image {
				// The image handler obtains its grant only after checking the
				// HMAC, attachment type and local file. Invalid URLs cannot
				// make a writer wait; profile images follow the ordinary path.
				return next(c)
			}
			regionalReadCounts.feedMisses.Add(1)
			start := time.Now()
			hash := hex.EncodeToString(key.auth[:])
			var grant struct {
				Viewer string `db:"viewer"`
				Leased bool   `db:"leased"`
			}
			err := h.DB.GetContext(r.Context(), &grant, `SELECT * FROM regional_feed_grant($1,$2,$3)`, hash, h.Config.Region, ip)
			if err == nil {
				v.session = &Session{UserID: grant.Viewer, TokenHash: hash, Region: h.Config.Region}
				if grant.Leased {
					v.until = start.Add(friendReadLifetime)
				}
			}
			return next(c)
		}
	}
}
