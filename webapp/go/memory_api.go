package isureal

import (
	"context"
	"net/http"
	"time"

	"github.com/jmoiron/sqlx"
	"github.com/labstack/echo/v5"
)

type getMemoriesRequest struct {
	Month          string     `query:"month"`
	BeforePostedAt *time.Time `query:"beforePostedAt"`
	BeforeID       string     `query:"beforeId"`
}

type memoryPostResponse struct {
	createPostResponse
	RealMojis []replaceRealMojiResponse `json:"realmojis"`
	Comments  []createCommentResponse   `json:"comments"`
}

type getMemoriesResponse struct {
	Items   []memoryPostResponse `json:"items"`
	HasMore bool                 `json:"hasMore"`
}

type getMemoriesByDateRequest struct {
	Date           string     `param:"date"`
	BeforePostedAt *time.Time `query:"beforePostedAt"`
	BeforeID       string     `query:"beforeId"`
}

type getMemoriesByDateResponse getMemoriesResponse

func getMemoriesHandler(h *HandlerContext) echo.HandlerFunc {
	return func(c *echo.Context) error {
		var input getMemoriesRequest
		if err := c.Bind(&input); err != nil {
			return err
		}
		start, err := time.Parse("2006-01", input.Month)
		if err != nil {
			return NewAPIError(http.StatusBadRequest, "invalid_memory_period")
		}
		response, err := listMemories(c.Request().Context(), h.DB, GetSession(c).UserID, start, start.AddDate(0, 1, 0), input.BeforePostedAt, input.BeforeID)
		if err != nil {
			return err
		}
		return c.JSON(http.StatusOK, response)
	}
}

func getMemoriesByDateHandler(h *HandlerContext) echo.HandlerFunc {
	return func(c *echo.Context) error {
		var input getMemoriesByDateRequest
		if err := c.Bind(&input); err != nil {
			return err
		}
		start, err := time.Parse(time.DateOnly, input.Date)
		if err != nil {
			return NewAPIError(http.StatusBadRequest, "invalid_memory_period")
		}
		response, err := listMemories(c.Request().Context(), h.DB, GetSession(c).UserID, start, start.AddDate(0, 0, 1), input.BeforePostedAt, input.BeforeID)
		if err != nil {
			return err
		}
		return c.JSON(http.StatusOK, getMemoriesByDateResponse(response))
	}
}

func listMemories(ctx context.Context, db sqlx.QueryerContext, userID string, start, end time.Time, beforeAt *time.Time, beforeID string) (getMemoriesResponse, error) {
	var response getMemoriesResponse
	if (beforeAt == nil) != (beforeID == "") {
		return response, NewAPIError(http.StatusBadRequest, "invalid_pagination")
	}
	posts, err := selectPostsWithAuthor(ctx, db, `
  AND p.author_id = $1 AND w.local_date >= $2 AND w.local_date < $3
  AND ($4::timestamptz IS NULL OR (p.posted_at, p.id) < ($4, $5))
  ORDER BY p.posted_at DESC, p.id DESC LIMIT 51
 `, userID, start, end, beforeAt, beforeID)
	if err != nil {
		return response, err
	}
	response.HasMore = len(posts) > 50
	if response.HasMore {
		posts = posts[:50]
	}
	postIDs := make([]string, len(posts))
	for i, post := range posts {
		postIDs[i] = post.ID
	}
	reactions, comments, err := loadMemoryInteractions(ctx, db, postIDs)
	if err != nil {
		return response, err
	}
	// Sign the whole page in one batch before building responses.
	requests := make([]signatureRequest, 0, 2*(len(posts)+len(reactions))+len(comments))
	for _, post := range posts {
		requests = appendImageSignatureRequests(requests, post.Author, post.ImageID)
		requests = appendProfileSignatureRequest(requests, post.Author)
	}
	for _, reaction := range reactions {
		requests = appendImageSignatureRequests(requests, reaction.Reactor, reaction.ImageID)
		requests = appendProfileSignatureRequest(requests, reaction.Reactor)
	}
	for _, comment := range comments {
		requests = appendProfileSignatureRequest(requests, comment.Author)
	}
	prefetchImageSignatures(ctx, requests)

	items := make([]memoryPostResponse, 0, len(posts))
	itemByPostID := make(map[string]*memoryPostResponse, len(posts))
	for _, post := range posts {
		items = append(items, memoryPostResponse{
			createPostResponse: newPostResponse(post.Post, post.Author),
			RealMojis:          make([]replaceRealMojiResponse, 0),
			Comments:           make([]createCommentResponse, 0),
		})
	}
	for i := range items {
		itemByPostID[items[i].ID] = &items[i]
	}
	for _, reaction := range reactions {
		item := itemByPostID[reaction.PostID]
		if len(item.RealMojis) < 50 {
			item.RealMojis = append(item.RealMojis, newRealMojiResponse(reaction.RealMoji, reaction.Reactor))
		}
	}
	for _, comment := range comments {
		item := itemByPostID[comment.PostID]
		if len(item.Comments) < 50 {
			item.Comments = append(item.Comments, newCommentResponse(comment.Comment, comment.Author))
		}
	}
	response.Items = items
	return response, nil
}

// loadMemoryInteractions reads current RealMojis (newest first) and comments
// (oldest first) for the given posts.
func loadMemoryInteractions(ctx context.Context, db sqlx.QueryerContext, postIDs []string) ([]RealMojiWithReactor, []CommentWithAuthor, error) {
	if len(postIDs) == 0 {
		return nil, nil, nil
	}
	query, args, err := sqlx.In(`AND r.post_id IN (?) ORDER BY r.created_at DESC, r.id DESC`, postIDs)
	if err != nil {
		return nil, nil, err
	}
	reactions, err := selectCurrentRealMojis(ctx, db, sqlx.Rebind(sqlx.DOLLAR, query), args...)
	if err != nil {
		return nil, nil, err
	}
	query, args, err = sqlx.In(`AND c.post_id IN (?) ORDER BY c.created_at, c.id`, postIDs)
	if err != nil {
		return nil, nil, err
	}
	comments, err := selectCommentsWithAuthor(ctx, db, sqlx.Rebind(sqlx.DOLLAR, query), args...)
	if err != nil {
		return nil, nil, err
	}
	return reactions, comments, nil
}
