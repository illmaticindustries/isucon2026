package isureal

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"net/url"
	"strconv"
	"sync"
	"time"
)

const imageURLLifetime = 24 * time.Hour
const imageSignatureIterations = 1000

// signatureCacheBucketWidth rounds the image signature expiry to a coarse time
// bucket. Without it exp changes every second, so the signature cache below is
// ineffective and every page render recomputes the 1000-round HMAC for each
// image. With a stable bucket the signature is reused within the bucket while
// exp stays roughly imageURLLifetime in the future.
const signatureCacheBucketWidth = time.Hour

var imageSigningKeyCache sync.Map

func imageSigningKey(secretKeyHash string) ([]byte, error) {
	if cached, ok := imageSigningKeyCache.Load(secretKeyHash); ok {
		return cached.([]byte), nil
	}
	key, err := hex.DecodeString(secretKeyHash)
	if err != nil || len(key) != sha256.Size {
		return nil, fmt.Errorf("invalid image signing key")
	}
	imageSigningKeyCache.Store(secretKeyHash, key)
	return key, nil
}

// A signature is a pure function of all three inputs. Including the key keeps
// cached signatures isolated across owners and key changes, including resets.
type signatureCacheKey struct {
	key     string
	imageID string
	exp     int64
}

// signatureCacheLimit must exceed the set of images signed within one expiry
// bucket. Each feed page signs up to 50 post images, RealMoji reactions and
// profile images, so the concurrent working set is on the order of the number
// of images reachable from the active users' feeds. At 1<<17 the working set
// overflowed and the generational rotation evicted entries before they were
// reused, so the 1000-round HMAC was recomputed for nearly every image
// (imageSignature was ~30% of CPU on asia1). The map only grows to the number
// of distinct images actually signed (~200 bytes each), so the higher limit
// only costs memory proportional to the real working set.
const signatureCacheLimit = 1 << 20

// signatureCache keeps the current and the previous generation of signatures so
// that overflow drops the older half instead of clearing everything. With a
// single map, reaching the limit wiped the whole cache; when the signed working
// set exceeds the limit this thrashes and the cache effectively never hits
// (observed via pprof: imageSignature was dominated by computeImageSignature).
var signatureCache = struct {
	sync.Mutex
	current  map[signatureCacheKey][sha256.Size]byte
	previous map[signatureCacheKey][sha256.Size]byte
}{current: make(map[signatureCacheKey][sha256.Size]byte)}

func loadImageSignature(cacheKey signatureCacheKey) ([sha256.Size]byte, bool) {
	signatureCache.Lock()
	defer signatureCache.Unlock()
	if value, ok := signatureCache.current[cacheKey]; ok {
		return value, true
	}
	value, ok := signatureCache.previous[cacheKey]
	return value, ok
}

func storeImageSignature(cacheKey signatureCacheKey, value [sha256.Size]byte) {
	signatureCache.Lock()
	defer signatureCache.Unlock()
	if len(signatureCache.current) >= signatureCacheLimit {
		signatureCache.previous = signatureCache.current
		signatureCache.current = make(map[signatureCacheKey][sha256.Size]byte)
	}
	signatureCache.current[cacheKey] = value
}

func imageSignature(key []byte, imageID string, exp int64) []byte {
	cacheKey := signatureCacheKey{key: string(key), imageID: imageID, exp: exp}
	if cached, ok := loadImageSignature(cacheKey); ok {
		return cached[:]
	}
	value := computeImageSignature(key, imageID, exp)
	var stored [sha256.Size]byte
	copy(stored[:], value)
	storeImageSignature(cacheKey, stored)
	return value
}

func computeImageSignature(key []byte, imageID string, exp int64) []byte {
	value := []byte(strconv.FormatInt(exp, 10) + ":" + imageID)
	// Reset reuses the keyed HMAC state; the 1000-round signature is unchanged.
	mac := hmac.New(sha256.New, key)
	for range imageSignatureIterations {
		mac.Reset()
		_, _ = mac.Write(value)
		value = mac.Sum(value[:0])
	}
	return value
}

// imageSignatureExpiry returns the signature expiry for now, floored to
// signatureCacheBucketWidth. It is always strictly in the future by between
// imageURLLifetime-signatureCacheBucketWidth and imageURLLifetime, so generated
// URLs stay valid for close to imageURLLifetime while being stable (and thus
// cacheable) for the whole bucket.
func imageSignatureExpiry(now time.Time) int64 {
	return now.Truncate(signatureCacheBucketWidth).Add(imageURLLifetime).Unix()
}

func signedImageURL(user User, imageID string) string {
	exp := imageSignatureExpiry(time.Now())
	query := url.Values{
		"exp": {strconv.FormatInt(exp, 10)},
		"sig": {base64.RawURLEncoding.EncodeToString(imageSignature(user.ImageKey, imageID, exp))},
	}
	return "/api/images/" + url.PathEscape(imageID) + "?" + query.Encode()
}
