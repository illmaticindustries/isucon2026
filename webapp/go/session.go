package isureal

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
	"github.com/labstack/echo/v5"
)

// sessionCacheTTL is short so that any state change made on another app server
// (login, logout, account deletion) is reflected within a bounded window.
// Invalidations performed by this process are applied immediately.
const sessionCacheTTL = 10 * time.Second

// sessionCacheSweepInterval is how many stores occur between opportunistic
// sweeps that drop expired entries for tokens that are never requested again.
const sessionCacheSweepInterval = 1024

type cachedSession struct {
	session  *Session
	expireAt time.Time
}

var (
	loginSessionCache    sync.Map
	loginSessionCacheOps atomic.Int64
)

func loadCachedSession(tokenHash string) (*Session, bool) {
	value, ok := loginSessionCache.Load(tokenHash)
	if !ok {
		return nil, false
	}
	entry := value.(cachedSession)
	if time.Now().After(entry.expireAt) {
		loginSessionCache.Delete(tokenHash)
		return nil, false
	}
	return entry.session, true
}

func storeCachedSession(tokenHash string, session *Session) {
	loginSessionCache.Store(tokenHash, cachedSession{
		session:  session,
		expireAt: time.Now().Add(sessionCacheTTL),
	})
	if loginSessionCacheOps.Add(1)%sessionCacheSweepInterval == 0 {
		sweepExpiredSessions(time.Now())
	}
}

func sweepExpiredSessions(now time.Time) {
	loginSessionCache.Range(func(key, value any) bool {
		if entry, ok := value.(cachedSession); ok && now.After(entry.expireAt) {
			loginSessionCache.Delete(key)
		}
		return true
	})
}

func invalidateCachedSession(tokenHash string) {
	loginSessionCache.Delete(tokenHash)
}

func invalidateCachedSessionsForUser(userID string) {
	loginSessionCache.Range(func(key, value any) bool {
		if entry, ok := value.(cachedSession); ok && entry.session.UserID == userID {
			loginSessionCache.Delete(key)
		}
		return true
	})
}

type Session struct {
	Region    string    `db:"region" json:"-"`
	Token     string    `db:"-" json:"-"`
	UserID    string    `db:"user_id"`
	TokenHash string    `db:"token_hash"`
	CreatedAt time.Time `db:"created_at"`
}

func GetSession(c *echo.Context) *Session {
	sess, ok := c.Get("session").(*Session)
	if !ok {
		return nil
	}
	return sess
}

func getSessionFromHeader(c *echo.Context, db *sqlx.DB) (*Session, error) {
	authHeader := c.Request().Header.Get(echo.HeaderAuthorization)
	if authHeader == "" {
		return nil, nil
	}
	if !strings.HasPrefix(authHeader, "Bearer ") {
		c.Logger().Warn("Invalid authorization header format(not started with Bearer)")
		return nil, NewAPIError(http.StatusUnauthorized, "invalid_session")
	}

	token := strings.TrimPrefix(authHeader, "Bearer ")

	tokenHashBytes := sha256.Sum256([]byte(token))
	tokenHash := hex.EncodeToString(tokenHashBytes[:])

	if session, ok := loadCachedSession(tokenHash); ok {
		return session, nil
	}

	var session Session
	err := db.GetContext(c.Request().Context(), &session, `
		SELECT s.user_id, s.token_hash, s.created_at, u.region
		FROM login_sessions s JOIN users u ON u.id = s.user_id
		WHERE s.token_hash = $1
		  AND s.active = true AND u.status = 'active'
	`, tokenHash)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("failed to get session: %w", err)
	}
	storeCachedSession(tokenHash, &session)
	return &session, nil
}

func SessionMiddleware(handlerContext *HandlerContext, authRequired bool) echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c *echo.Context) error {
			session, err := getSessionFromHeader(c, handlerContext.DB)
			if err != nil {
				return err
			}
			if session == nil && authRequired {
				return NewAPIError(http.StatusUnauthorized, "invalid_session")
			}
			c.Set("session", session)
			return next(c)
		}
	}
}

func InvalidateLoginSession(db *sqlx.DB, userID string, tokenHash string) error {
	tx, err := db.Beginx()
	if err != nil {
		return fmt.Errorf("failed to begin transaction: %w", err)
	}
	defer tx.Rollback()

	var lockedUserID string
	if err := tx.Get(&lockedUserID, `SELECT id FROM users WHERE id = $1 FOR NO KEY UPDATE`, userID); err != nil {
		return fmt.Errorf("failed to lock user: %w", err)
	}

	if _, err := tx.Exec(`
		UPDATE login_sessions
		SET active = false, invalidated_at = clock_timestamp(), reason = 'logout'
		WHERE user_id = $1 AND active = true AND token_hash = $2	
	`, userID, tokenHash); err != nil {
		return fmt.Errorf("failed to invalidate sessions: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("failed to commit transaction: %w", err)
	}

	invalidateCachedSession(tokenHash)

	return nil
}

// 2026-09-03: リファクタリング後にこの関数の利用がないようです。消すのはいかがでしょうか
func CreateLoginSession(db *sqlx.DB, userID string) (*Session, error) {
	ctx := context.Background()
	tx, err := db.BeginTxx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to begin transaction: %w", err)
	}
	defer tx.Rollback()
	session, err := createLoginSessionTx(ctx, tx, userID)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("failed to commit transaction: %w", err)
	}
	return session, nil
}

func createLoginSessionTx(ctx context.Context, tx *sqlx.Tx, userID string) (*Session, error) {
	randomToken := uuid.NewString()
	tokenHashBytes := sha256.Sum256([]byte(randomToken))
	tokenHash := hex.EncodeToString(tokenHashBytes[:])
	var lockedUserID string
	if err := tx.GetContext(ctx, &lockedUserID, `SELECT id FROM users WHERE id = $1 AND status = 'active' FOR NO KEY UPDATE`, userID); err != nil {
		return nil, fmt.Errorf("failed to lock user: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
  UPDATE login_sessions
  SET active = false, invalidated_at = clock_timestamp(), reason = 'replaced'
  WHERE user_id = $1 AND active = true
 `, userID); err != nil {
		return nil, fmt.Errorf("failed to invalidate sessions: %w", err)
	}
	sessionID := uuid.NewString()
	if _, err := tx.ExecContext(ctx, `
  INSERT INTO login_sessions (id, user_id, token_hash)
  VALUES ($1, $2, $3)
 `, sessionID, userID, tokenHash); err != nil {
		return nil, fmt.Errorf("failed to insert session: %w", err)
	}
	session, err := getLoginSession(ctx, tx, sessionID)
	if err != nil {
		return nil, err
	}
	session.Token = randomToken
	return session, nil
}

func getLoginSession(ctx context.Context, db sqlx.QueryerContext, sessionID string) (*Session, error) {
	var session Session
	if err := sqlx.GetContext(ctx, db, &session, `SELECT user_id, token_hash, created_at FROM login_sessions WHERE id = $1`, sessionID); err != nil {
		return nil, fmt.Errorf("failed to get session: %w", err)
	}
	return &session, nil
}
