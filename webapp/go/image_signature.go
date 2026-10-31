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
	"sync/atomic"
	"time"
)

const imageURLLifetime = 24 * time.Hour
const imageSignatureIterations = 1
const imageSignatureCacheLimit = 1 << 16

type imageSignatureCacheKey struct {
	imageID string
	exp     int64
}

var imageSignatureCache sync.Map
var imageSignatureCacheSize atomic.Int64

func imageSigningKey(secretKeyHash string) ([]byte, error) {
	key, err := hex.DecodeString(secretKeyHash)
	if err != nil || len(key) != sha256.Size {
		return nil, fmt.Errorf("invalid image signing key")
	}
	return key, nil
}

func imageSignature(key []byte, imageID string, exp int64) []byte {
	cacheKey := imageSignatureCacheKey{imageID: imageID, exp: exp}
	if cached, ok := imageSignatureCache.Load(cacheKey); ok {
		return cached.([]byte)
	}
	value := []byte(strconv.FormatInt(exp, 10) + ":" + imageID)
	for range imageSignatureIterations {
		mac := hmac.New(sha256.New, key)
		mac.Write(value)
		value = mac.Sum(nil)
	}
	if imageSignatureCacheSize.Add(1) > imageSignatureCacheLimit {
		imageSignatureCache.Clear()
		imageSignatureCacheSize.Store(0)
	}
	imageSignatureCache.Store(cacheKey, value)
	return value
}

func signedImageURL(user User, imageID string) string {
	exp := time.Now().Add(imageURLLifetime).Unix()
	query := url.Values{
		"exp": {strconv.FormatInt(exp, 10)},
		"sig": {base64.RawURLEncoding.EncodeToString(imageSignature(user.ImageKey, imageID, exp))},
	}
	return "/api/images/" + url.PathEscape(imageID) + "?" + query.Encode()
}
