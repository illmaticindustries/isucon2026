package isureal

import (
	"crypto/hmac"
	"crypto/sha256"
	"hash/maphash"
	"sync/atomic"
)

// These bounded hints select a SQL projection only. Every request still verifies
// the current database owner, live reference, generation, signature and expiry.
type issuedImageURL struct {
	id  string
	exp int64
	sig [sha256.Size]byte
}

var imageHintSeed = maphash.MakeSeed()
var issuedImages [16384]atomic.Pointer[issuedImageURL]
var warmImages [16384]atomic.Uint64

func rememberImageURL(id string, exp int64, sig []byte) {
	slot := &issuedImages[maphash.String(imageHintSeed, id)%uint64(len(issuedImages))]
	if old := slot.Load(); old != nil && old.id == id && old.exp == exp && hmac.Equal(sig, old.sig[:]) {
		return
	}
	slot.Store(&issuedImageURL{id: id, exp: exp, sig: [sha256.Size]byte(sig)})
}

func hintWarmImage(id string) {
	hash := maphash.String(imageHintSeed, id)
	warmImages[hash%uint64(len(warmImages))].Store(hash)
}

func prefetchImageBody(id string, exp int64, sig []byte) bool {
	hash := maphash.String(imageHintSeed, id)
	if warmImages[hash%uint64(len(warmImages))].Load() == hash {
		return false
	}
	proof := issuedImages[hash%uint64(len(issuedImages))].Load()
	return proof != nil && proof.id == id && proof.exp == exp && hmac.Equal(proof.sig[:], sig)
}
