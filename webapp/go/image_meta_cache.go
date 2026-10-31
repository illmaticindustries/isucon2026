package isureal

import (
	"sync"
	"sync/atomic"
	"time"
)

// imageMetaEntry is the per-image metadata used by GET /api/images.
type imageMetaEntry struct {
	epoch         int64
	secretKeyHash string
	generation    int64
	createdAt     time.Time
	mediaType     string
	valid         bool
	expiresAt     time.Time
}

// Short-lived cache for the owner/validity lookup that would otherwise run on
// every image view. A burst of views of the same immutable image (four views per
// user per posting window, plus the post-detail rechecks) shares one query.
//
// The validity flag can flip when a post is deleted or a RealMoji is replaced.
// The specification allows a deleted post's image to remain reachable for five
// seconds, so entries expire far earlier (imageMetaTTL) than that grace. The
// immutable fields (owner key, created_at, media_type) never change, and the
// cache is cleared on initialize, which is the only time the image epoch moves.
var imageMetaTTL = 1 * time.Second

const imageMetaCacheLimit = 1 << 16

var imageMetaCache sync.Map
var imageMetaCacheSize atomic.Int64

// imageMetaEpoch is bumped by any write that can change an image's validity
// (post deletion, RealMoji replacement/deletion, profile image change, account
// deletion). Entries recorded under an older epoch are ignored, so a change made
// in this process is reflected on the very next request instead of after the TTL.
var imageMetaEpoch atomic.Int64

// invalidateImageMeta makes every cached entry stale without freeing memory.
func invalidateImageMeta() {
	imageMetaEpoch.Add(1)
}

func loadImageMeta(now time.Time, id string) (imageMetaEntry, bool) {
	value, ok := imageMetaCache.Load(id)
	if !ok {
		return imageMetaEntry{}, false
	}
	entry := value.(imageMetaEntry)
	if entry.epoch != imageMetaEpoch.Load() || now.After(entry.expiresAt) {
		return imageMetaEntry{}, false
	}
	return entry, true
}

func storeImageMeta(id string, entry imageMetaEntry) {
	entry.epoch = imageMetaEpoch.Load()
	entry.expiresAt = time.Now().Add(imageMetaTTL)
	if imageMetaCacheSize.Add(1) > imageMetaCacheLimit {
		imageMetaCache.Clear()
		imageMetaCacheSize.Store(0)
	}
	imageMetaCache.Store(id, entry)
}

func clearImageMeta() {
	imageMetaEpoch.Add(1)
	imageMetaCache.Clear()
	imageMetaCacheSize.Store(0)
}
