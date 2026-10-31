package isureal

import (
	"net/http/httptest"
	"testing"
	"time"

	"github.com/labstack/echo/v5"
)

func TestSessionCacheableRoutePatterns(t *testing.T) {
	cases := []struct {
		method  string
		pattern string
		want    bool
	}{
		{"GET", "/api/users", false},
		{"POST", "/api/posts", false},
		{"POST", "/api/friend-requests", false},
		{"POST", "/api/friend-requests/:requestId/:action", false},
		// Every other route is served in-region and may be cached.
		{"GET", "/api/users/:userId", true},
		{"POST", "/api/posts/:postId/comments", true},
		{"PATCH", "/api/me", true},
		{"GET", "/api/feeds/:feed", true},
		{"PUT", "/api/me/profile-image", true},
	}
	e := echo.New()
	for _, tc := range cases {
		req := httptest.NewRequest(tc.method, "/", nil)
		c := e.NewContext(req, httptest.NewRecorder())
		c.SetPath(tc.pattern)
		if got := sessionCacheable(c); got != tc.want {
			t.Errorf("%s %s: got %v want %v", tc.method, tc.pattern, got, tc.want)
		}
	}
}

func TestSessionCacheStoreLoadAndInvalidate(t *testing.T) {
	clearSessionCache()
	defer clearSessionCache()

	storeCachedSession("token-1", &Session{UserID: "u1", TokenHash: "token-1", Region: "asia"})
	got, ok := loadCachedSession("token-1")
	if !ok || got.UserID != "u1" || got.Region != "asia" {
		t.Fatalf("load: %v %+v", ok, got)
	}
	// Mutating the loaded copy must not change the cached value.
	got.Region = "europe"
	if again, _ := loadCachedSession("token-1"); again.Region != "asia" {
		t.Fatal("cached session was mutated through the returned copy")
	}
	if _, ok := loadCachedSession("missing"); ok {
		t.Fatal("unexpected cache hit")
	}

	storeCachedSession("token-2", &Session{UserID: "u2", TokenHash: "token-2", Region: "europe"})
	invalidateCachedSessionsForUser("u1")
	if _, ok := loadCachedSession("token-1"); ok {
		t.Fatal("user invalidation left token-1")
	}
	if _, ok := loadCachedSession("token-2"); !ok {
		t.Fatal("user invalidation removed another user's token")
	}

	invalidateCachedSession("token-2")
	if _, ok := loadCachedSession("token-2"); ok {
		t.Fatal("token invalidation left token-2")
	}
}

func TestSessionCacheExpiry(t *testing.T) {
	clearSessionCache()
	defer clearSessionCache()
	sessionCache.Store("expired", cachedSession{
		session:  &Session{UserID: "u1"},
		expireAt: time.Now().Add(-time.Second),
	})
	if _, ok := loadCachedSession("expired"); ok {
		t.Fatal("expired entry served")
	}
}
