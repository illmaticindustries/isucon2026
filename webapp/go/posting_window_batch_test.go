package isureal

import (
	"context"
	"os"
	"testing"
	"time"
)

func TestBatchWindowUsesCurrentRegionAndDate(t *testing.T) {
	db := isolatedWriteTestDB(t)
	schema, err := os.ReadFile("../sql/001-schema.sql")
	if err != nil {
		t.Fatal(err)
	}
	db.MustExec(string(schema))
	db.MustExec(`INSERT INTO users(id,username,secret_key_hash,display_name,region) VALUES('a','a','00','A','asia'),('b','b','00','B','europe');
 INSERT INTO posting_windows(id,region,local_date,starts_at,on_time_deadline) VALUES ('asia','asia',current_date,now(),now()),('eu','europe',current_date,now(),now());
 INSERT INTO posts(id,author_id,posting_window_id,image_id,visibility,location_kind,posted_at,deleted_at) VALUES ('1','b','asia','1','public','none',now(),now()),('2','b','eu','2','public','none',now(),null);`)
	h := HandlerContext{DB: db}
	read := func() []*postingWindowState {
		t.Helper()
		s, err := h.readPostingWindows(context.Background(), []string{"b", "missing", "a", "b"})
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	s := read()
	if s[0].ID != "eu" || s[0].Active != 1 || s[0].Deleted != 1 || s[1] != nil || s[2].Active != 0 || s[0] != s[3] {
		t.Fatalf("incorrect batch: %+v", s)
	}
	db.MustExec("UPDATE users SET region='asia' WHERE id='b'")
	if read()[0].Region != "asia" {
		t.Fatal("stale region")
	}
	db.MustExec("UPDATE posting_windows SET ended_at=$1 WHERE region='asia'", time.Now())
	if read()[0] != nil {
		t.Fatal("stale open window")
	}
}
