package isureal

import (
	"database/sql"
	"errors"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/jmoiron/sqlx"
	"github.com/labstack/echo/v5"
	"golang.org/x/text/cases"
	"golang.org/x/text/unicode/norm"
)

const normalizeSearchCacheLimit = 1 << 16

var normalizeSearchCache sync.Map
var normalizeSearchCacheSize atomic.Int64

type searchUsersRequest struct {
	Q              string `query:"q"`
	BeforeUsername string `query:"beforeUsername"`
	BeforeID       string `query:"beforeId"`
}

type searchUsersResponse struct {
	Items   []relationshipUserResponse `json:"items"`
	HasMore bool                       `json:"hasMore"`
}

func normalizeSearchText(value string) string {
	if cached, ok := normalizeSearchCache.Load(value); ok {
		return cached.(string)
	}
	normalized := norm.NFC.String(cases.Fold().String(norm.NFC.String(value)))
	if normalizeSearchCacheSize.Add(1) > normalizeSearchCacheLimit {
		normalizeSearchCache.Clear()
		normalizeSearchCacheSize.Store(0)
	}
	normalizeSearchCache.Store(value, normalized)
	return normalized
}

func searchUsersHandler(h *HandlerContext) echo.HandlerFunc {
	return func(c *echo.Context) error {
		var input searchUsersRequest
		if err := c.Bind(&input); err != nil {
			return err
		}
		if !validUserText(input.Q, 1, 100) {
			return NewAPIError(http.StatusBadRequest, "invalid_relationship")
		}
		if (input.BeforeUsername == "") != (input.BeforeID == "") {
			return NewAPIError(http.StatusBadRequest, "invalid_pagination")
		}
		ctx := c.Request().Context()
		viewer := GetSession(c).UserID
		blocked, err := getBlockedUserIDs(ctx, h.ReadDB(), viewer)
		if err != nil {
			return err
		}
		query := normalizeSearchText(input.Q)
		// PostgreSQL text cannot contain NUL, but the API accepts it in q. The
		// original Go comparison therefore returns no matches, not a SQL error.
		if strings.ContainsRune(query, '\x00') {
			return c.JSON(http.StatusOK, searchUsersResponse{Items: []relationshipUserResponse{}})
		}
		// Match on narrow rows and stop at 51 matches; decoding every column of every
		// active user dominated asia1 CPU. Both queries share one snapshot.
		tx, err := h.ReadDB().BeginTxx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
		if err != nil {
			return err
		}
		defer tx.Rollback()
		rows, err := tx.QueryContext(ctx, searchCandidateQuery, viewer, input.BeforeUsername, input.BeforeID, query)
		if err != nil {
			return err
		}
		defer rows.Close()
		ids := make([]string, 0, 51)
		for rows.Next() {
			var id, username, displayName string
			if err := rows.Scan(&id, &username, &displayName); err != nil {
				return err
			}
			if blocked[id] {
				continue
			}
			if !strings.Contains(normalizeSearchText(username), query) && !strings.Contains(normalizeSearchText(displayName), query) {
				continue
			}
			ids = append(ids, id)
			if len(ids) == 51 {
				break
			}
		}
		if err := rows.Close(); err != nil {
			return err
		}
		if err := rows.Err(); err != nil {
			return err
		}
		users := make([]User, 0, len(ids))
		if len(ids) > 0 {
			var fetched []User
			if err := tx.SelectContext(ctx, &fetched, `SELECT `+strings.Join(userColumns, ", ")+` FROM users WHERE id = ANY($1)`, ids); err != nil {
				return err
			}
			byID := make(map[string]User, len(fetched))
			for _, user := range fetched {
				byID[user.ID] = user
			}
			for _, id := range ids {
				if user, ok := byID[id]; ok {
					users = append(users, user)
				}
			}
		}
		if err := tx.Commit(); err != nil {
			return err
		}
		// Release the database connection before computing response signatures.
		items := make([]relationshipUserResponse, 0, len(users))
		for _, user := range users {
			if err := user.setImageKey(); err != nil {
				return err
			}
			if user.AccountType != "certified_public" {
				user.Bio = ""
			}
			items = append(items, newRelationshipUserResponse(user))
		}
		hasMore := len(items) > 50
		if hasMore {
			items = items[:50]
		}
		return c.JSON(http.StatusOK, searchUsersResponse{Items: items, HasMore: hasMore})
	}
}

type getUserRequest struct {
	UserID string `param:"userId"`
}

type getUserResponse relationshipUserResponse

func getUserHandler(h *HandlerContext) echo.HandlerFunc {
	return func(c *echo.Context) error {
		var input getUserRequest
		if err := c.Bind(&input); err != nil {
			return err
		}
		ctx := c.Request().Context()
		viewer := GetSession(c).UserID
		// A region that reaches the shared database over an inter-region link
		// pays a round trip per statement. The profile, whether the viewer
		// blocked the target, and whether they are friends are constants of one
		// primary snapshot, so fetch them in a single statement.
		var row struct {
			User
			IsBlocked bool `db:"is_blocked"`
			IsFriend  bool `db:"is_friend"`
		}
		err := sqlx.GetContext(ctx, h.ReadDB(), &row, `
  SELECT `+strings.Join(userColumns, ", ")+`,
    EXISTS(SELECT 1 FROM block_relations b
      WHERE (b.blocker_id=$2 AND b.blocked_id=u.id)
         OR (b.blocker_id=u.id AND b.blocked_id=$2)) AS is_blocked,
    EXISTS(SELECT 1 FROM friendships f
      WHERE (f.user_low_id=$2 AND f.user_high_id=u.id)
         OR (f.user_low_id=u.id AND f.user_high_id=$2)) AS is_friend
  FROM users u WHERE u.id=$1 AND u.status='active'
  `, input.UserID, viewer)
		if errors.Is(err, sql.ErrNoRows) {
			return NewAPIError(http.StatusNotFound, "relationship_not_found")
		}
		if err != nil {
			return err
		}
		if err := row.User.setImageKey(); err != nil {
			return err
		}
		if row.IsBlocked {
			return NewAPIError(http.StatusNotFound, "relationship_not_found")
		}
		if row.User.AccountType != "certified_public" && !row.IsFriend {
			row.User.Bio = ""
		}
		return c.JSON(http.StatusOK, getUserResponse(newRelationshipUserResponse(row.User)))
	}
}

func newRelationshipUserResponse(user User) relationshipUserResponse {
	return relationshipUserResponse{ID: user.ID, Username: user.Username, DisplayName: user.DisplayName, ProfileImageURL: newGetMeResponse(user).ProfileImageURL, Bio: user.Bio, AccountType: user.AccountType}
}
