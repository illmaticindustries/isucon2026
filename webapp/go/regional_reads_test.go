package isureal

import (
	"crypto/sha256"
	"github.com/labstack/echo/v5"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestRegionalReadsDoNotExtendOldGrants(t *testing.T) {
	var rc regionalReads
	key := regionalReadKey{uri: [32]byte{1}, auth: [32]byte{2}}
	ip, otherIP := [32]byte{3}, [32]byte{4}
	page := &getFeedResponse{}
	lease := regionalRead{key: key, ip: ip, feed: page, until: time.Now().Add(time.Second)}
	rc.store(lease)
	if _, ok := rc.load(key, otherIP, true); ok {
		t.Fatal("unverified IP read a cached page")
	}
	otherUser := key
	otherUser.auth = [32]byte{5}
	if _, ok := rc.load(otherUser, ip, true); ok {
		t.Fatal("page leaked across sessions")
	}
	if got, ok := rc.load(key, ip, true); !ok || got.feed != page || got.until != lease.until {
		t.Fatal("hit lost or renewed its grant")
	}
	rc.clear()
	lease.until = time.Now().Add(-time.Second)
	rc.store(lease)
	if _, ok := rc.load(key, ip, true); ok {
		t.Fatal("late fill revived an expired lease")
	}
}

func TestRegionalImageFillIsBounded(t *testing.T) {
	var rc regionalReads
	for i := byte(0); i < 4; i++ {
		key := regionalReadKey{uri: [32]byte{i}}
		if !rc.startImageFill(key) || rc.startImageFill(key) {
			t.Fatal("duplicate or missing image work")
		}
	}
	next := regionalReadKey{uri: [32]byte{4}}
	if rc.startImageFill(next) {
		t.Fatal("exceeded image fill limit")
	}
	rc.finishImageFill(regionalReadKey{})
	if !rc.startImageFill(next) {
		t.Fatal("completed fill did not free a slot")
	}
}

func TestRegionalImageHitChecksExactURLExpiryAndIP(t *testing.T) {
	files := &imageFileStore{root: t.TempDir()}
	const file = "photo.jpg"
	if err := os.WriteFile(filepath.Join(files.root, file), []byte("image"), 0600); err != nil {
		t.Fatal(err)
	}
	h := &HandlerContext{ImageFiles: files}
	e := echo.New()
	e.IPExtractor = extractClientIP
	e.Use(regionalReadMiddleware(h))
	e.GET("/api/images/:imageId", func(c *echo.Context) error { return c.NoContent(http.StatusForbidden) })
	const uri = "/api/images/id?exp=9999999999&sig=signed"
	const ip = "198.51.100.23"
	entry := regionalRead{key: regionalReadKey{uri: sha256.Sum256([]byte(uri))}, ip: sha256.Sum256([]byte(ip)), file: file, mediaType: "image/jpeg", until: time.Now().Add(time.Second), exp: 9999999999}
	h.regional.store(entry)
	check := func(path, client, byteRange string, want int) {
		t.Helper()
		r := httptest.NewRequest("GET", path, nil)
		r.Header.Set("X-Client-IP", client)
		r.Header.Set("X-Isureal-Image-Accel", "v1")
		if byteRange != "" {
			r.Header.Set("Range", byteRange)
		}
		w := httptest.NewRecorder()
		e.ServeHTTP(w, r)
		if w.Code != want {
			t.Fatalf("got %d want %d", w.Code, want)
		}
	}
	check(uri, ip, "", 200)
	check(uri+"&sig=other", ip, "", 403)
	check(uri, "198.51.100.99", "", 403)
	check(uri, ip, "bytes=0-2", 403)
	entry.exp = time.Now().Unix()
	h.regional.store(entry)
	check(uri, ip, "", 403)
}
