package isureal

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/jmoiron/sqlx"
	"github.com/labstack/echo/v5"
)

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

// A forwarded listener must read the database even when this process holds a
// cached session: the region's own process may have logged that token out.
func TestForwardedListenerBypassesSessionCache(t *testing.T) {
	clearSessionCache()
	defer clearSessionCache()
	db, err := sqlx.Open("pgx", "postgres://user:pass@127.0.0.1:1/unreachable?connect_timeout=1")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	token := "stale-token"
	hash := sha256.Sum256([]byte(token))
	tokenHash := hex.EncodeToString(hash[:])
	storeCachedSession(tokenHash, &Session{UserID: "u1", TokenHash: tokenHash, Region: "europe"})

	newContext := func() *echo.Context {
		req := httptest.NewRequest(http.MethodPost, "/api/posts/p1/comments", nil)
		req.Header.Set(echo.HeaderAuthorization, "Bearer "+token)
		return echo.New().NewContext(req, httptest.NewRecorder())
	}
	if session, err := getSessionFromHeader(newContext(), db, false); err != nil || session == nil || session.UserID != "u1" {
		t.Fatalf("own-region listener: session %+v err %v, want cached session", session, err)
	}
	if session, err := getSessionFromHeader(newContext(), db, true); err == nil {
		t.Fatalf("forwarded listener served %+v from the cache", session)
	}
}
