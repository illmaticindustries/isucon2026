package isureal

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"fmt"
	"strconv"
	"sync"
	"testing"
)

func originalSignature(key []byte, id string, exp int64) []byte {
	value := []byte(strconv.FormatInt(exp, 10) + ":" + id)
	for i := 0; i < 1000; i++ {
		mac := hmac.New(sha256.New, key)
		mac.Write(value)
		value = mac.Sum(nil)
	}
	return value
}

func TestSignatureCompatibility(t *testing.T) {
	for _, key := range [][]byte{bytes.Repeat([]byte{1}, 32), bytes.Repeat([]byte{2}, 32), nil} {
		for _, id := range []string{"", "a", "12345678-1234-1234-1234-123456789abc"} {
			for _, exp := range []int64{0, -1, 1793440800} {
				want := originalSignature(key, id, exp)
				if got := computeImageSignature(key, id, exp); !bytes.Equal(got, want) {
					t.Fatal("uncached signature changed")
				}
				for i := 0; i < 2; i++ {
					got := imageSignature(key, id, exp)
					if !bytes.Equal(got, want) {
						t.Fatal("cached signature changed")
					}
					got[0] ^= 255 // Callers cannot mutate a stored signature.
				}
			}
		}
	}
}

func TestSignatureParallelAndBounded(t *testing.T) {
	var wg sync.WaitGroup
	for n := 0; n < 16; n++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			key := bytes.Repeat([]byte{byte(n)}, 32)
			want := originalSignature(key, "same-image", 42)
			for i := 0; i < 30; i++ {
				if !bytes.Equal(imageSignature(key, "same-image", 42), want) {
					t.Error("key isolation failed")
				}
			}
		}(n)
	}
	wg.Wait()
	// Fill the bounded cache without thousands of expensive computations.
	signatureCache.Lock()
	for i := 0; i < signatureCacheLimit; i++ {
		signatureCache.values[signatureCacheKey{imageID: fmt.Sprint(i)}] = [32]byte{}
	}
	signatureCache.Unlock()
	imageSignature([]byte("eviction-key"), "eviction", 99)
	signatureCache.Lock()
	defer signatureCache.Unlock()
	if len(signatureCache.values) > signatureCacheLimit {
		t.Fatal("cache exceeds bound")
	}
}

func BenchmarkSignatureOriginal(b *testing.B) {
	key := bytes.Repeat([]byte{1}, 32)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		originalSignature(key, "12345678-1234-1234-1234-123456789abc", 1793440800)
	}
}
func BenchmarkSignatureUncached(b *testing.B) {
	key := bytes.Repeat([]byte{1}, 32)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		computeImageSignature(key, "12345678-1234-1234-1234-123456789abc", 1793440800)
	}
}
func BenchmarkSignatureCached(b *testing.B) {
	key := bytes.Repeat([]byte{1}, 32)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		imageSignature(key, "12345678-1234-1234-1234-123456789abc", 1793440800)
	}
}
