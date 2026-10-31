package isureal

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

func resetPrewarmed() {
	prewarmed.Lock()
	defer prewarmed.Unlock()
	clear(prewarmed.buckets)
	clear(prewarmed.complete)
}

func prewarmTestItems(n int) []prewarmItem {
	items := make([]prewarmItem, n)
	for i := range items {
		key := string(bytes.Repeat([]byte{byte(i % 3)}, sha256.Size))
		items[i] = prewarmItem{key: key, imageID: fmt.Sprintf("image-%d", i)}
	}
	return items
}

func TestPrewarmedSignaturesMatchComputed(t *testing.T) {
	resetPrewarmed()
	defer resetPrewarmed()
	items := prewarmTestItems(1500)
	const exp = 1793440800
	if !computePrewarmBucket(context.Background(), items, exp, func() bool { return true }) {
		t.Fatal("prewarm did not finish")
	}
	for _, item := range items {
		want := computeImageSignature([]byte(item.key), item.imageID, exp)
		got, ok := loadPrewarmedSignature(signatureCacheKey{key: item.key, imageID: item.imageID, exp: exp})
		if !ok || !bytes.Equal(got[:], want) {
			t.Fatalf("%s: prewarmed signature differs", item.imageID)
		}
		if !bytes.Equal(imageSignature([]byte(item.key), item.imageID, exp), want) {
			t.Fatalf("%s: imageSignature differs", item.imageID)
		}
	}
	// Another expiry is not served from this bucket.
	if _, ok := loadPrewarmedSignature(signatureCacheKey{key: items[0].key, imageID: items[0].imageID, exp: exp + 3600}); ok {
		t.Fatal("bucket leaked into another expiry")
	}
}

func TestPrewarmWaitsWhileBusy(t *testing.T) {
	resetPrewarmed()
	defer resetPrewarmed()
	items := prewarmTestItems(prewarmChunk * 2)
	var idle atomic.Bool
	done := make(chan bool)
	go func() { done <- computePrewarmBucket(context.Background(), items, 1, idle.Load) }()
	time.Sleep(700 * time.Millisecond)
	prewarmed.RLock()
	computed := len(prewarmed.buckets[1])
	prewarmed.RUnlock()
	if computed != 0 {
		t.Fatalf("computed %d signatures while busy", computed)
	}
	idle.Store(true)
	if !<-done {
		t.Fatal("prewarm did not finish once idle")
	}
	prewarmed.RLock()
	computed = len(prewarmed.buckets[1])
	prewarmed.RUnlock()
	if computed != len(items) {
		t.Fatalf("computed %d of %d", computed, len(items))
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if computePrewarmBucket(ctx, items, 2, func() bool { return false }) {
		t.Fatal("cancelled prewarm reported completion")
	}
}

func TestPrewarmPersistence(t *testing.T) {
	resetPrewarmed()
	defer resetPrewarmed()
	dir := t.TempDir()
	items := prewarmTestItems(700)
	const exp = 1793444400
	computePrewarmBucket(context.Background(), items, exp, func() bool { return true })
	if err := savePrewarmBucket(dir, exp); err != nil {
		t.Fatal(err)
	}
	stale := filepath.Join(dir, fmt.Sprintf("signatures-%d.gob", exp-3600))
	if err := os.WriteFile(stale, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	resetPrewarmed()
	loadPrewarmFiles(dir, exp)
	if !prewarmBucketComplete(exp) {
		t.Fatal("loaded bucket not marked complete")
	}
	for _, item := range items {
		got, ok := loadPrewarmedSignature(signatureCacheKey{key: item.key, imageID: item.imageID, exp: exp})
		if !ok || !bytes.Equal(got[:], computeImageSignature([]byte(item.key), item.imageID, exp)) {
			t.Fatalf("%s: restored signature differs", item.imageID)
		}
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatal("stale bucket file was not removed")
	}
	dropPrewarmedBefore(exp + 1)
	if prewarmBucketComplete(exp) {
		t.Fatal("expired bucket still marked complete")
	}
}

func TestServerIdleFollowsActivity(t *testing.T) {
	lastRequestAt.Store(time.Now().UnixNano())
	if serverIdle() {
		t.Fatal("idle right after a request")
	}
	lastRequestAt.Store(time.Now().Add(-prewarmIdleAfter - time.Second).UnixNano())
	if !serverIdle() {
		t.Fatal("not idle after the quiet period")
	}
	lastRequestAt.Store(0)
}
