package isureal

import (
	"os"
	"testing"
	"time"
)

func TestImageObjectIndexSchema(t *testing.T) {
	db := openLockTestDB(t)
	db.MustExec(`CREATE TABLE image_objects (id varchar(36) NOT NULL, owner_user_id text, data bytea, media_type text,
 created_at timestamptz NOT NULL, PRIMARY KEY (id, created_at)) PARTITION BY RANGE (created_at)`)
	db.MustExec(`CREATE TABLE image_objects_a PARTITION OF image_objects FOR VALUES FROM ('2025-01-01') TO ('2026-01-01')`)
	db.MustExec(`CREATE TABLE image_objects_b PARTITION OF image_objects FOR VALUES FROM ('2026-01-01') TO ('2027-01-01')`)
	db.MustExec(`INSERT INTO image_objects(id, created_at) VALUES ('old-a', '2025-06-01'), ('old-b', '2026-06-01')`)
	schema, err := os.ReadFile("../sql/008-image-object-index.sql")
	if err != nil {
		t.Fatal(err)
	}
	for range 2 { // Deploys apply the file every time.
		db.MustExec(string(schema))
	}
	db.MustExec(`CREATE TABLE image_objects_c PARTITION OF image_objects FOR VALUES FROM ('2027-01-01') TO ('2028-01-01')`)
	db.MustExec(`INSERT INTO image_objects(id, created_at) VALUES ('new-b', '2026-10-31'), ('new-c', '2027-02-01')`)

	want := map[string]time.Time{
		"old-a": time.Date(2025, 6, 1, 0, 0, 0, 0, time.UTC), "old-b": time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC),
		"new-b": time.Date(2026, 10, 31, 0, 0, 0, 0, time.UTC), "new-c": time.Date(2027, 2, 1, 0, 0, 0, 0, time.UTC),
	}
	var rows []struct {
		ID        string    `db:"id"`
		CreatedAt time.Time `db:"created_at"`
	}
	if err := db.Select(&rows, `SELECT id, created_at FROM image_object_index`); err != nil {
		t.Fatal(err)
	}
	if len(rows) != len(want) {
		t.Fatalf("index has %d rows, want %d: %+v", len(rows), len(want), rows)
	}
	for _, row := range rows {
		if !row.CreatedAt.Equal(want[row.ID]) {
			t.Fatalf("%s: created_at %v, want %v", row.ID, row.CreatedAt, want[row.ID])
		}
	}
	var triggers int
	if err := db.Get(&triggers, `SELECT count(*) FROM pg_trigger WHERE tgrelid = 'image_objects'::regclass AND tgname = 'image_object_index_insert'`); err != nil || triggers != 1 {
		t.Fatalf("triggers=%d err=%v", triggers, err)
	}

	// With created_at the lookup touches one partition instead of all of them.
	locks := func(query string, args ...any) int {
		tx := db.MustBegin()
		defer tx.Rollback()
		var id string
		if err := tx.Get(&id, query, args...); err != nil {
			t.Fatal(err)
		}
		var n int
		if err := tx.Get(&n, `SELECT count(*) FROM pg_locks WHERE pid = pg_backend_pid() AND locktype = 'relation' AND relation::regclass::text LIKE 'image_objects%'`); err != nil {
			t.Fatal(err)
		}
		return n
	}
	all := locks(`SELECT id FROM image_objects WHERE id = $1`, "new-c")
	pruned := locks(`SELECT id FROM image_objects WHERE id = $1 AND created_at = $2`, "new-c", want["new-c"])
	if pruned >= all {
		t.Fatalf("pruned lookup took %d image_objects locks, id-only took %d", pruned, all)
	}
}
