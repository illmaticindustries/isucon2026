package isureal

import (
	"context"
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
		query := normalizeSearchText(input.Q)
		// PostgreSQL text cannot contain NUL, but the API accepts it in q. The
		// original Go comparison therefore returns no matches, not a SQL error.
		if strings.ContainsRune(query, '\x00') {
			return c.JSON(http.StatusOK, searchUsersResponse{Items: []relationshipUserResponse{}})
		}
		users, candidates, err := searchUsersPage(ctx, h.DB, searchCandidateQuery+" LIMIT 51", viewer, input, query)
		if err != nil {
			return err
		}
		// Stale derived fields can admit nonmatches. Only when those fill a page
		// do we repeat the whole read from a fresh snapshot without the SQL cap.
		if candidates == 51 && len(users) < 51 {
			users, _, err = searchUsersPage(ctx, h.DB, searchCandidateQuery, viewer, input, query)
			if err != nil {
				return err
			}
		}
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

// Blocks, ordering and response fields come from the same statement snapshot.
func searchUsersPage(ctx context.Context, db *sqlx.DB, statement, viewer string, input searchUsersRequest, query string) ([]User, int, error) {
	rows, err := db.QueryxContext(ctx, statement, viewer, input.BeforeUsername, input.BeforeID, query)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	users := make([]User, 0, 51)
	var candidates int
	for rows.Next() {
		var user User
		if err := rows.StructScan(&user); err != nil {
			return nil, 0, err
		}
		candidates++
		if strings.Contains(normalizeSearchText(user.Username), query) || strings.Contains(normalizeSearchText(user.DisplayName), query) {
			users = append(users, user)
			if len(users) == 51 {
				break
			}
		}
	}
	if err := rows.Close(); err != nil {
		return nil, 0, err
	}
	return users, candidates, rows.Err()
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
		err := sqlx.GetContext(ctx, h.DB, &row, `
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
