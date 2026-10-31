package isureal

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func resetSignatureCache() {
	signatureCache.Lock()
	defer signatureCache.Unlock()
	signatureCache.current = make(map[signatureCacheKey][sha256.Size]byte)
	signatureCache.previous = nil
}

func remoteSignTestRequests(n int) []signatureRequest {
	requests := make([]signatureRequest, n)
	for i := range requests {
		requests[i] = signatureRequest{key: bytes.Repeat([]byte{byte(i%5 + 1)}, sha256.Size), imageID: fmt.Sprintf("12345678-1234-1234-1234-%012d", i)}
	}
	return requests
}

func withRemoteSigners(t *testing.T, addresses []string) {
	t.Helper()
	previous := remoteSigners
	remoteSigners = addresses
	t.Cleanup(func() { remoteSigners = previous; resetSignatureCache() })
	resetSignatureCache()
}

func TestSignatureRequestRoundTrip(t *testing.T) {
	requests := remoteSignTestRequests(40)
	exp, decoded, err := decodeSignatureRequest(bytes.NewReader(encodeSignatureRequest(1793440800, requests)))
	if err != nil || exp != 1793440800 || len(decoded) != len(requests) {
		t.Fatalf("exp=%d len=%d err=%v", exp, len(decoded), err)
	}
	for i := range requests {
		if !bytes.Equal(decoded[i].key, requests[i].key) || decoded[i].imageID != requests[i].imageID {
			t.Fatalf("item %d differs", i)
		}
	}
	if _, _, err := decodeSignatureRequest(bytes.NewReader(encodeSignatureRequest(1, requests)[:30])); err == nil {
		t.Fatal("truncated request accepted")
	}
}

func TestPrefetchFillsCacheWithRemoteSignatures(t *testing.T) {
	server := httptest.NewServer(signatureServiceHandler("127.0.0.1"))
	defer server.Close()
	address := strings.TrimPrefix(server.URL, "http://")
	// Two signers exercise sharding; both point at the same test service.
	withRemoteSigners(t, []string{address, address})
	requests := remoteSignTestRequests(100)
	requests = append(requests, requests[:10]...) // duplicates are signed once
	prefetchImageSignatures(context.Background(), requests)
	exp := imageSignatureExpiry(time.Now())
	for _, request := range requests {
		want := computeImageSignature(request.key, request.imageID, exp)
		got, ok := loadImageSignature(signatureCacheKey{key: string(request.key), imageID: request.imageID, exp: exp})
		if !ok || !bytes.Equal(got[:], want) {
			t.Fatalf("%s: remote signature missing or different", request.imageID)
		}
		if !bytes.Equal(imageSignature(request.key, request.imageID, exp), want) {
			t.Fatalf("%s: imageSignature differs", request.imageID)
		}
	}
}

func TestSignatureServiceRejectsOtherPeers(t *testing.T) {
	server := httptest.NewServer(signatureServiceHandler("192.168.1.10"))
	defer server.Close()
	withRemoteSigners(t, []string{strings.TrimPrefix(server.URL, "http://")})
	requests := remoteSignTestRequests(32)
	prefetchImageSignatures(context.Background(), requests)
	exp := imageSignatureExpiry(time.Now())
	if _, ok := loadImageSignature(signatureCacheKey{key: string(requests[0].key), imageID: requests[0].imageID, exp: exp}); ok {
		t.Fatal("signature cached from a rejected request")
	}
	// Signing still works locally.
	if !bytes.Equal(imageSignature(requests[0].key, requests[0].imageID, exp), computeImageSignature(requests[0].key, requests[0].imageID, exp)) {
		t.Fatal("local fallback differs")
	}
}

func TestPrefetchSkipsSmallBatchesAndUnreachableSigners(t *testing.T) {
	withRemoteSigners(t, []string{"127.0.0.1:1"})
	started := time.Now()
	prefetchImageSignatures(context.Background(), remoteSignTestRequests(64))
	if elapsed := time.Since(started); elapsed > remoteSignTimeout+time.Second {
		t.Fatalf("unreachable signer blocked for %s", elapsed)
	}
	signatureCache.Lock()
	cached := len(signatureCache.current)
	signatureCache.Unlock()
	if cached != 0 {
		t.Fatalf("cached %d signatures from an unreachable signer", cached)
	}
}

func TestSignatureRequestsSkipMissingKeysAndLongIDs(t *testing.T) {
	key := bytes.Repeat([]byte{1}, sha256.Size)
	profile := "p"
	requests := appendImageSignatureRequests(nil, User{ImageKey: key}, "image")
	requests = appendImageSignatureRequests(requests, User{}, "no-key")
	requests = appendImageSignatureRequests(requests, User{ImageKey: key}, strings.Repeat("x", 256))
	requests = appendProfileSignatureRequest(requests, User{ImageKey: key})
	requests = appendProfileSignatureRequest(requests, User{ImageKey: key, ProfileImageID: &profile})
	if len(requests) != 2 || requests[0].imageID != "image" || requests[1].imageID != "p" {
		t.Fatalf("unexpected requests: %+v", requests)
	}
}
