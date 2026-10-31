package isureal

import (
	"sync"
	"time"
)

// imageLocator caches the immutable location of an image: its partition key
// created_at and its owner's signing key. image_objects is partitioned by
// created_at, so a lookup by id alone probes every partition; with the cached
// created_at the per-request metadata query can include it and prune to a single
// partition. Authorization, validity and generation are still checked on every
// request, never from this cache.
type imageLocator struct {
	createdAt time.Time
	ownerKey  string
}

var imageLocators sync.Map

func loadImageLocator(imageID string) (imageLocator, bool) {
	value, ok := imageLocators.Load(imageID)
	if !ok {
		return imageLocator{}, false
	}
	return value.(imageLocator), true
}

func storeImageLocator(imageID string, locator imageLocator) {
	imageLocators.Store(imageID, locator)
}
