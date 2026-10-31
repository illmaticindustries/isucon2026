package isureal

import (
	"bytes"
	"crypto/sha256"
	"net/url"
	"strconv"
	"testing"
	"time"
)

func TestImageSignatureExpiryStableWithinBucket(t *testing.T) {
	base := time.Date(2026, 10, 31, 2, 0, 0, 0, time.UTC)
	want := imageSignatureExpiry(base)
	for _, d := range []time.Duration{0, time.Second, 59 * time.Minute, 59*time.Minute + 59*time.Second} {
		if got := imageSignatureExpiry(base.Add(d)); got != want {
			t.Fatalf("exp changed within bucket at +%s: got %d want %d", d, got, want)
		}
	}
	if imageSignatureExpiry(base.Add(time.Hour)) == want {
		t.Fatal("exp did not advance after one bucket")
	}
}

func TestImageSignatureExpiryInFuture(t *testing.T) {
	for _, now := range []time.Time{
		time.Date(2026, 10, 31, 2, 0, 0, 0, time.UTC),
		time.Date(2026, 10, 31, 2, 59, 59, 0, time.UTC),
	} {
		exp := imageSignatureExpiry(now)
		if exp <= now.Unix() {
			t.Fatalf("exp %d is not in the future of %d", exp, now.Unix())
		}
		if max := now.Add(imageURLLifetime).Unix(); exp > max {
			t.Fatalf("exp %d exceeds now+lifetime %d", exp, max)
		}
	}
}

func TestSignedImageURLStableWithinBucket(t *testing.T) {
	user := User{ImageKey: bytes.Repeat([]byte{7}, sha256.Size)}
	a := signedImageURL(user, "image-1")
	b := signedImageURL(user, "image-1")
	if a != b {
		t.Fatalf("URL not stable within a bucket: %q vs %q", a, b)
	}
	u, err := url.Parse(a)
	if err != nil {
		t.Fatal(err)
	}
	exp, err := strconv.ParseInt(u.Query().Get("exp"), 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	if exp != imageSignatureExpiry(time.Now()) {
		t.Fatalf("exp %d does not match bucketed expiry", exp)
	}
}
