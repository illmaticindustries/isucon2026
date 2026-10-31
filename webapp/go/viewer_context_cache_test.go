package isureal

import (
	"testing"
	"time"
)

func TestViewerContextCacheStoreInvalidateAndExpiry(t *testing.T) {
	clearViewerContextCache()
	defer clearViewerContextCache()

	storeViewerContext("u1", viewerContext{User: User{ID: "u1"}, Friends: map[string]bool{"u2": true}})
	got, ok := loadCachedViewerContext("u1")
	if !ok || !got.Friends["u2"] {
		t.Fatalf("load: %v %+v", ok, got)
	}
	if _, ok := loadCachedViewerContext("missing"); ok {
		t.Fatal("unexpected cache hit")
	}

	storeViewerContext("u2", viewerContext{User: User{ID: "u2"}})
	invalidateViewerContexts("u1", "u2")
	if _, ok := loadCachedViewerContext("u1"); ok {
		t.Fatal("pair invalidation left u1")
	}
	if _, ok := loadCachedViewerContext("u2"); ok {
		t.Fatal("pair invalidation left u2")
	}

	storeViewerContext("u3", viewerContext{User: User{ID: "u3"}})
	invalidateViewerContext("u3")
	if _, ok := loadCachedViewerContext("u3"); ok {
		t.Fatal("single invalidation left u3")
	}

	viewerContextCache.Store("expired", cachedViewerContext{
		context:   viewerContext{User: User{ID: "u4"}},
		expiresAt: time.Now().Add(-time.Second),
	})
	if _, ok := loadCachedViewerContext("expired"); ok {
		t.Fatal("expired entry served")
	}
}
