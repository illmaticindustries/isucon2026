package isureal

import (
	"context"
	"os"
	"testing"

	"github.com/jmoiron/sqlx"
)

func TestSessionReadBatchPostgres(t *testing.T) {
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
	db.MustExec(`CREATE TEMP TABLE users(id text, region text, status text);
		CREATE TEMP TABLE login_sessions(user_id text, token_hash text, created_at timestamptz, active bool);
		INSERT INTO users VALUES('u','asia','active'),('gone','asia','deleted');
		INSERT INTO login_sessions VALUES('u','live',now(),true),('u','replaced',now(),false),('gone','retired',now(),true)`)
	h := HandlerContext{DB: db}
	ctx := context.Background()
	hashes := []string{"replaced", "live", "missing", "retired", "live"}
	sessions, err := h.readSessions(ctx, hashes)
	if err != nil || sessions[0] != nil || sessions[2] != nil || sessions[3] != nil ||
		sessions[1] == nil || sessions[4] == nil || sessions[1] == sessions[4] || sessions[1].Region != "asia" {
		t.Fatalf("bad mixed session lookup: %v", err)
	}
	db.MustExec(`UPDATE users SET region='europe' WHERE id='u'`)
	sessions, err = h.readSessions(ctx, hashes)
	if err != nil || sessions[1].Region != "europe" {
		t.Fatal("moved session not refreshed", err)
	}
	db.MustExec(`UPDATE login_sessions SET active=false`)
	sessions, err = h.readSessions(ctx, hashes)
	if err != nil || sessions[1] != nil || sessions[4] != nil {
		t.Fatal("logout not immediately visible", err)
	}
}
