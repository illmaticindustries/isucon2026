package isureal

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/labstack/echo/v5"
)

func TestFriendRequestResponsesMatchCommittedState(t *testing.T) {
	db := isolatedWriteTestDB(t)
	schema, err := os.ReadFile("../sql/001-schema.sql")
	if err != nil {
		t.Fatal(err)
	}
	db.MustExec(string(schema))
	db.MustExec(`INSERT INTO users(id,username,secret_key_hash,display_name,region)
 SELECT u,u,repeat('12',32),u,'asia' FROM unnest(ARRAY['a','b','c']) AS u`)

	h := &HandlerContext{DB: db}
	e := echo.New()
	e.HTTPErrorHandler = APIErrorHandler
	e.Use(func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c *echo.Context) error {
			c.Set("session", &Session{UserID: c.Request().Header.Get("X-Test-Actor")})
			return next(c)
		}
	})
	const route = "/api/friend-requests"
	e.POST(route, createFriendRequestHandler(h))
	e.POST(route+"/:requestId/:action", transitionFriendRequestHandler(h))
	post := func(actor, path, body string, wantCode int, wantStatus string) string {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Test-Actor", actor)
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, req)
		if rec.Code != wantCode {
			t.Fatalf("%s %s: got %d %s, want %d", actor, path, rec.Code, rec.Body.String(), wantCode)
		}
		if wantCode >= 400 {
			var result APIErrorResponse
			if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil || result.Error != wantStatus {
				t.Fatalf("unexpected error response: %s", rec.Body.String())
			}
			return ""
		}
		var result createFriendRequestResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		stored, err := getFriendRequest(t.Context(), db, result.RelationID)
		if err != nil || result.Status != wantStatus || stored.Status != wantStatus || !result.UpdatedAt.Equal(stored.UpdatedAt) {
			t.Fatalf("response %+v, stored %+v, want status %s, err %v", result, stored, wantStatus, err)
		}
		return result.RelationID
	}

	first := post("a", route, `{"recipientId":"b"}`, 201, "pending")
	post("a", route, `{"recipientId":"b"}`, 409, "relationship_conflict")
	if reverse := post("b", route, `{"recipientId":"a"}`, 201, "accepted"); reverse != first {
		t.Fatalf("reciprocal request created %s instead of accepting %s", reverse, first)
	}
	var source string
	if err := db.Get(&source, `SELECT source_request_id FROM friendships WHERE user_low_id='a' AND user_high_id='b'`); err != nil || source != first {
		t.Fatalf("friendship source %q, want %q, err %v", source, first, err)
	}
	post("b", route, `{"recipientId":"a"}`, 409, "relationship_conflict")
	pending := post("a", route, `{"recipientId":"c"}`, 201, "pending")
	post("a", route+"/"+pending+"/accept", "", 403, "relationship_action_forbidden")
	if accepted := post("c", route+"/"+pending+"/accept", "", 200, "accepted"); accepted != pending {
		t.Fatalf("accepted request %s, want %s", accepted, pending)
	}
	db.MustExec(`INSERT INTO block_relations(id,blocker_id,blocked_id) VALUES('block','c','b')`)
	post("b", route, `{"recipientId":"c"}`, 409, "relationship_conflict")
	post("c", route, `{"recipientId":"b"}`, 409, "relationship_conflict")
}
