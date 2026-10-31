package isureal

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/labstack/echo/v5"
)

func TestIssuedImageColdLookup(t *testing.T) {
	id := uuid.NewString()
	owner := User{ImageKey: bytes.Repeat([]byte{4}, 32)}
	raw := signedImageURL(owner, id)
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	exp, _ := strconv.ParseInt(u.Query().Get("exp"), 10, 64)
	sig, _ := base64.RawURLEncoding.DecodeString(u.Query().Get("sig"))
	if !prefetchImageBody(id, exp, sig) || prefetchImageBody(id, exp+1, sig) || prefetchImageBody(id, exp, make([]byte, 32)) || prefetchImageBody(uuid.NewString(), exp, sig) {
		t.Fatal("cold reads must use the exact issued URL")
	}
	hintWarmImage(id)
	if prefetchImageBody(id, exp, sig) {
		t.Fatal("warm image requested its bytes early")
	}
}

func TestIssuedImageRechecksDatabase(t *testing.T) {
	db := isolatedWriteTestDB(t)
	for _, name := range []string{"001-schema.sql", "005-image-cache.sql"} {
		applyPhotoTestSQL(t, db, name)
	}
	e := echo.New()
	e.HTTPErrorHandler = APIErrorHandler
	e.GET("/api/images/:imageId", getImageHandler(&HandlerContext{DB: db}))
	owner := User{ImageKey: bytes.Repeat([]byte{7}, 32)}
	db.MustExec(`INSERT INTO users(id,username,display_name,secret_key_hash,region) VALUES('owner','owner','Owner',$1,'asia')`, hex.EncodeToString(owner.ImageKey))
	get := func(raw string, code int, want string) {
		t.Helper()
		w := httptest.NewRecorder()
		e.ServeHTTP(w, httptest.NewRequest("GET", raw, nil))
		if w.Code != code || want != "" && w.Body.String() != want {
			t.Fatalf("got %d %s", w.Code, w.Body.String())
		}
	}
	makeURL := func(body string, profile bool) string {
		id := uuid.NewString()
		db.MustExec(`INSERT INTO image_objects(id,owner_user_id,media_type,data) VALUES($1,'owner','image/jpeg',$2)`, id, []byte(body))
		if profile {
			db.MustExec("UPDATE users SET profile_image_id=$1", id)
		}
		return signedImageURL(owner, id)
	}
	raw := makeURL("original", true)
	get(raw, 200, "original")
	get(raw, 200, "original")
	db.MustExec("UPDATE users SET profile_image_id=NULL")
	get(raw, 403, "")
	get(makeURL("unreferenced", false), 403, "")
	raw = makeURL("cold", true)
	db.MustExec("UPDATE users SET secret_key_hash=$1", strings.Repeat("08", 32))
	get(raw, 403, "")
}
