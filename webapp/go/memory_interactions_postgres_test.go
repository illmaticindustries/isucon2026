package isureal

import (
	"context"
	"fmt"
	"os"
	"testing"

	"github.com/jmoiron/sqlx"
)

func TestMemoryInteractionsPerPostLimit(t *testing.T) {
	dsn := os.Getenv("ISUREAL_TEST_DB_DSN")
	if dsn == "" {
		t.Skip("requires PostgreSQL")
	}
	db, err := sqlx.Connect("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	db.MustExec(`CREATE TEMP TABLE users(id text, username text, secret_key_hash text, display_name text,
    bio text,profile_image_id text,region text,account_type text,status text);
    CREATE TEMP TABLE realmojis(id text,post_id text,reactor_id text,image_id text,reaction_type text,
    created_at timestamptz,replaced_at timestamptz,deleted_at timestamptz);
    CREATE TEMP TABLE comments(id text,post_id text,author_id text,body text,created_at timestamptz,deleted_at timestamptz);
    INSERT INTO users VALUES ('live','live',repeat('12',32),'live','',NULL,'asia','regular','active'),
                             ('gone','gone',repeat('34',32),'gone','',NULL,'asia','regular','deleted');
    INSERT INTO realmojis SELECT p||lpad(i::text,2,'0'),p,CASE WHEN i=55 THEN 'gone' ELSE 'live' END,
      p||i,'like',now(),CASE WHEN i IN (1,54) THEN now() END,CASE WHEN i=0 THEN now() END
      FROM unnest(ARRAY['first','second']) p CROSS JOIN generate_series(0,55) i;
    INSERT INTO comments SELECT id,post_id,reactor_id,'body',created_at,COALESCE(deleted_at,replaced_at) FROM realmojis`)
	items := make([]memoryPostResponse, 3)
	items[0].ID, items[1].ID, items[2].ID = "second", "empty", "first"
	if err := loadMemoryInteractions(context.Background(), db, items); err != nil {
		t.Fatal(err)
	}
	for _, item := range items {
		if item.ID == "empty" {
			if len(item.RealMojis)+len(item.Comments) != 0 {
				t.Fatal("unexpected interactions for empty post")
			}
			continue
		}
		if len(item.RealMojis) != 50 || len(item.Comments) != 50 {
			t.Fatalf("wrong page size for %s", item.ID)
		}
		for i := range 50 {
			if item.RealMojis[i].ID != fmt.Sprintf("%s%02d", item.ID, 53-i) || item.Comments[i].ID != fmt.Sprintf("%s%02d", item.ID, 2+i) {
				t.Fatalf("wrong interaction order at %s[%d]", item.ID, i)
			}
		}
	}
}
