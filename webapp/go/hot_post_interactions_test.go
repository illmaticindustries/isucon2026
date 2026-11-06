package isureal

import (
	"bytes"
	"context"
	"encoding/json"
	"image"
	"image/jpeg"
	"mime/multipart"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/labstack/echo/v5"
)

func TestPopularPostConcurrentInteractions(t *testing.T) {
	db := isolatedWriteTestDB(t)
	for _, name := range []string{"001-schema.sql", "005-image-cache.sql"} {
		schema, err := os.ReadFile("../sql/" + name)
		if err != nil {
			t.Fatal(err)
		}
		db.MustExec(string(schema))
	}
	db.MustExec(`INSERT INTO users(id,username,secret_key_hash,display_name,region)
 SELECT x,x,repeat('12',32),x,'asia' FROM unnest(ARRAY['alice','bob','creator']) x;
 INSERT INTO posting_windows(id,region,local_date,starts_at,on_time_deadline) VALUES ('today','asia',current_date,now(),now()+interval '1 hour');
 INSERT INTO posts(id,author_id,posting_window_id,image_id,visibility,location_kind,posted_at)
 SELECT id,id,'today',id,'public','hidden',clock_timestamp() FROM users`)
	h := &HandlerContext{DB: db}
	e := echo.New()
	e.HTTPErrorHandler = APIErrorHandler
	e.Use(func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c *echo.Context) error { c.Set("session", &Session{UserID: "alice"}); return next(c) }
	})
	e.POST("/api/posts/:postId/comments", createCommentHandler(h))
	e.PUT("/api/posts/:postId/realmojis", replaceRealMojiHandler(h))
	e.PUT("/api/follows/:userId", followHandler(h))
	follow := func(target string, want int) {
		t.Helper()
		w := httptest.NewRecorder()
		e.ServeHTTP(w, httptest.NewRequest("PUT", "/api/follows/"+target, nil))
		if w.Code != want {
			t.Fatalf("follow %s: %d %s", target, w.Code, w.Body.String())
		}
	}
	follow("creator", 409)
	db.MustExec(`UPDATE users SET account_type='certified_public' WHERE id='creator'`)
	follow("creator", 204)
	follow("creator", 409)

	comment := func(post string, want int) {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		req := httptest.NewRequest("POST", "/api/posts/"+post+"/comments", strings.NewReader(`{"body":"hello"}`)).WithContext(ctx)
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		e.ServeHTTP(w, req)
		if w.Code != want {
			t.Fatalf("comment %s: %d %s", post, w.Code, w.Body.String())
		}
		if want == 201 {
			var got createCommentResponse
			if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil || got.Body != "hello" || got.Author.ID != "alice" || got.ID == "" {
				t.Fatalf("comment response %s", w.Body.String())
			}
		}
	}
	held, err := beginRelationshipTx(context.Background(), db, "bob", "creator")
	if err != nil {
		t.Fatal(err)
	}
	defer held.Rollback()
	comment("creator", 201)
	if err := held.Rollback(); err != nil {
		t.Fatal(err)
	}
	comment("alice", 201)
	var form bytes.Buffer
	writer := multipart.NewWriter(&form)
	if err := writer.WriteField("reactionType", "like"); err != nil {
		t.Fatal(err)
	}
	file, err := writer.CreateFormFile("image", "reaction.jpg")
	if err != nil {
		t.Fatal(err)
	}
	if err := jpeg.Encode(file, image.NewGray(image.Rect(0, 0, 8, 8)), nil); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		req := httptest.NewRequest("PUT", "/api/posts/creator/realmojis", bytes.NewReader(form.Bytes()))
		req.Header.Set("Content-Type", writer.FormDataContentType())
		w := httptest.NewRecorder()
		e.ServeHTTP(w, req)
		var got replaceRealMojiResponse
		if w.Code != 201 || json.Unmarshal(w.Body.Bytes(), &got) != nil || got.Reactor.ID != "alice" || got.ImageURL == "" {
			t.Fatalf("reaction: %d %s", w.Code, w.Body.String())
		}
	}
	var active int
	if err := db.Get(&active, `SELECT count(*) FROM realmojis WHERE replaced_at IS NULL`); err != nil || active != 1 {
		t.Fatalf("active reactions %d: %v", active, err)
	}
	db.MustExec(`INSERT INTO block_relations(id,blocker_id,blocked_id) VALUES('block','creator','alice')`)
	comment("creator", 403)
	db.MustExec(`DELETE FROM follows`)
	follow("creator", 409)
}
