package isureal

import (
	"testing"
	"time"
)

func TestImageLocatorCache(t *testing.T) {
	at := time.Date(2026, 10, 31, 0, 0, 0, 0, time.UTC)
	storeImageLocator("loc-1", imageLocator{createdAt: at, ownerKey: "key-1"})
	got, ok := loadImageLocator("loc-1")
	if !ok || !got.createdAt.Equal(at) || got.ownerKey != "key-1" {
		t.Fatalf("locator not stored: %+v ok=%v", got, ok)
	}
	if _, ok := loadImageLocator("loc-missing"); ok {
		t.Fatal("unknown locator must not be served")
	}
}
