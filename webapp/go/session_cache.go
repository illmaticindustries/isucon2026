package isureal

import (
	"sync"
	"sync/atomic"
	"time"

	"github.com/labstack/echo/v5"
)

// crossProcessRoutes are forwarded by nginx to another region's listener
// (see etc/nginx/sites-available/isureal-*.conf). Their session must be read
// from the database so that a logout, deletion or region change performed on
// this host is visible there. Every other route is served in-region and its
// session read may be cached.
var crossProcessRoutes = map[string]struct{}{
	"GET /api/users":                               {},
	"POST /api/posts":                              {},
	"POST /api/friend-requests":                    {},
	"POST /api/friend-requests/:requestId/:action": {},
}

func sessionCacheable(c *echo.Context) bool {
	_, cross := crossProcessRoutes[c.Request().Method+" "+c.Path()]
	return !cross
}

// sessionCacheTTL bounds how long a session read may be reused. Every mutation
// this process performs (login, logout, account deletion, profile/region
// change) invalidates the affected entries immediately; the TTL only covers
// changes made elsewhere that this process cannot see.
const sessionCacheTTL = 10 * time.Second

// sessionCacheSweepInterval is how many stores occur between opportunistic
// sweeps that drop entries for tokens that are never requested again.
const sessionCacheSweepInterval = 1024

type cachedSession struct {
	session  *Session
	expireAt time.Time
}

var (
	sessionCache    sync.Map
	sessionCacheOps atomic.Int64
)

// loadCachedSession returns a copy so callers cannot mutate the cached value.
func loadCachedSession(tokenHash string) (*Session, bool) {
	value, ok := sessionCache.Load(tokenHash)
	if !ok {
		return nil, false
	}
	entry := value.(cachedSession)
	if time.Now().After(entry.expireAt) {
		sessionCache.Delete(tokenHash)
		return nil, false
	}
	session := *entry.session
	return &session, true
}

func storeCachedSession(tokenHash string, session *Session) {
	stored := *session
	sessionCache.Store(tokenHash, cachedSession{session: &stored, expireAt: time.Now().Add(sessionCacheTTL)})
	if sessionCacheOps.Add(1)%sessionCacheSweepInterval == 0 {
		sweepExpiredSessions(time.Now())
	}
}

func sweepExpiredSessions(now time.Time) {
	sessionCache.Range(func(key, value any) bool {
		if entry, ok := value.(cachedSession); ok && now.After(entry.expireAt) {
			sessionCache.Delete(key)
		}
		return true
	})
}

func invalidateCachedSession(tokenHash string) {
	sessionCache.Delete(tokenHash)
}

func invalidateCachedSessionsForUser(userID string) {
	sessionCache.Range(func(key, value any) bool {
		if entry, ok := value.(cachedSession); ok && entry.session.UserID == userID {
			sessionCache.Delete(key)
		}
		return true
	})
}

func clearSessionCache() {
	sessionCache.Range(func(key, _ any) bool {
		sessionCache.Delete(key)
		return true
	})
}
