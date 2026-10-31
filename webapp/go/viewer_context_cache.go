package isureal

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jmoiron/sqlx"
)

// The friends / friends-of-friends / mutual / blocked graph changes only when a
// relationship or the user's own status/region changes, so it is cached per
// viewer. has_posted_today is deliberately not cached: it flips on every post
// and on every posting-window switch, and is cheap to re-check per request.
//
// viewerContextTTL bounds how long a change made by another process (a friend
// accepting a request on a different regional listener) may go unnoticed. Every
// change made in this process invalidates the affected viewers immediately.
const viewerContextTTL = 2 * time.Second

type cachedViewerContext struct {
	context   viewerContext
	expiresAt time.Time
}

var (
	viewerContextCache    sync.Map
	viewerContextCacheOps atomic.Int64
)

func loadCachedViewerContext(id string) (viewerContext, bool) {
	value, ok := viewerContextCache.Load(id)
	if !ok {
		return viewerContext{}, false
	}
	entry := value.(cachedViewerContext)
	if time.Now().After(entry.expiresAt) {
		viewerContextCache.Delete(id)
		return viewerContext{}, false
	}
	return entry.context, true
}

func storeViewerContext(id string, viewer viewerContext) {
	viewerContextCache.Store(id, cachedViewerContext{context: viewer, expiresAt: time.Now().Add(viewerContextTTL)})
	if viewerContextCacheOps.Add(1)%1024 == 0 {
		sweepViewerContexts(time.Now())
	}
}

func sweepViewerContexts(now time.Time) {
	viewerContextCache.Range(func(key, value any) bool {
		if entry, ok := value.(cachedViewerContext); ok && now.After(entry.expiresAt) {
			viewerContextCache.Delete(key)
		}
		return true
	})
}

func invalidateViewerContext(id string) {
	viewerContextCache.Delete(id)
}

func invalidateViewerContexts(ids ...string) {
	for _, id := range ids {
		viewerContextCache.Delete(id)
	}
}

func clearViewerContextCache() {
	viewerContextCache.Range(func(key, _ any) bool {
		viewerContextCache.Delete(key)
		return true
	})
}

// loadViewerContextCached reuses the relationship graph while re-checking the
// volatile has_posted_today flag, so a burst of ranked-feed reads for one viewer
// shares the heavy CTE.
func loadViewerContextCached(ctx context.Context, db sqlx.QueryerContext, viewerID string) (viewerContext, error) {
	viewer, ok := loadCachedViewerContext(viewerID)
	if !ok {
		loaded, err := loadViewerContext(ctx, db, viewerID)
		if err != nil {
			return loaded, err
		}
		storeViewerContext(viewerID, loaded)
		return loaded, nil
	}
	hasPosted, err := hasPostedToday(ctx, db, viewerID, viewer.Region)
	if err != nil {
		return viewer, err
	}
	viewer.HasPostedToday = hasPosted
	return viewer, nil
}

func hasPostedToday(ctx context.Context, db sqlx.QueryerContext, viewerID, region string) (bool, error) {
	var posted bool
	err := sqlx.GetContext(ctx, db, &posted, `
  SELECT EXISTS(
    SELECT 1 FROM posts p
    JOIN posting_windows posted ON posted.id = p.posting_window_id
    JOIN posting_windows current ON current.region = $2 AND current.ended_at IS NULL
                                AND current.local_date = posted.local_date
    WHERE p.author_id = $1 AND p.deleted_at IS NULL
  )`, viewerID, region)
	return posted, err
}
