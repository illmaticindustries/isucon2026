package isureal

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jmoiron/sqlx"
)

// openLockTestDB creates an isolated schema so that several connections share the tables.
func openLockTestDB(t *testing.T) *sqlx.DB {
	t.Helper()
	dsn := os.Getenv("ISUREAL_TEST_DB_DSN")
	if dsn == "" {
		t.Skip("set ISUREAL_TEST_DB_DSN for PostgreSQL integration tests")
	}
	admin, err := sqlx.Connect("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	schema := fmt.Sprintf("locktest_%d", time.Now().UnixNano())
	admin.MustExec(`CREATE SCHEMA ` + schema)
	t.Cleanup(func() { admin.MustExec(`DROP SCHEMA ` + schema + ` CASCADE`); admin.Close() })
	separator := "?"
	if strings.Contains(dsn, "?") {
		separator = "&"
	}
	db, err := sqlx.Connect("pgx", dsn+separator+"search_path="+schema)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	db.MustExec(`CREATE TABLE users (id text PRIMARY KEY, username text NOT NULL, secret_key_hash text NOT NULL DEFAULT '', status text NOT NULL DEFAULT 'active')`)
	db.MustExec(`CREATE TABLE login_sessions (id text PRIMARY KEY, user_id text NOT NULL REFERENCES users(id), token_hash text NOT NULL,
 active boolean NOT NULL DEFAULT true, created_at timestamptz NOT NULL DEFAULT clock_timestamp(), invalidated_at timestamptz, reason text)`)
	db.MustExec(`INSERT INTO users(id, username, status) VALUES ('A','a','active'),('B','b','active'),('C','c','active'),('D','d','deleted')`)
	return db
}

// blocks reports whether f is still waiting after d (its context is then cancelled).
func blocks(t *testing.T, d time.Duration, f func(context.Context) error) bool {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), d)
	defer cancel()
	err := f(ctx)
	if err == nil {
		return false
	}
	if ctx.Err() == nil {
		t.Fatalf("unexpected error: %v", err)
	}
	return true
}

func TestRelationshipLocksArePerPair(t *testing.T) {
	db := openLockTestDB(t)
	held, err := beginRelationshipTx(context.Background(), db, "A", "B")
	if err != nil {
		t.Fatal(err)
	}
	defer held.Rollback()

	// Another pair with the same user proceeds.
	if blocks(t, time.Second, func(ctx context.Context) error {
		tx, err := beginRelationshipTx(ctx, db, "C", "A")
		if err == nil {
			tx.Rollback()
		}
		return err
	}) {
		t.Fatal("relationship with another user waited for the A-B pair")
	}
	// The same pair, in either order, is serialized.
	if !blocks(t, 300*time.Millisecond, func(ctx context.Context) error {
		tx, err := beginRelationshipTx(ctx, db, "B", "A")
		if err == nil {
			tx.Rollback()
		}
		return err
	}) {
		t.Fatal("same pair was not serialized")
	}
	// Deactivating a participant still waits for the relationship change. The
	// update runs in a transaction that is rolled back even if the cancel is late.
	update, err := db.BeginTxx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if !blocks(t, 300*time.Millisecond, func(ctx context.Context) error {
		_, err := update.ExecContext(ctx, `UPDATE users SET status='deleted' WHERE id='A'`)
		return err
	}) {
		t.Fatal("user update did not wait for the relationship change")
	}
	update.Rollback()
	if err := held.Commit(); err != nil {
		t.Fatal(err)
	}
	tx, err := beginRelationshipTx(context.Background(), db, "B", "A")
	if err != nil {
		t.Fatalf("same pair after commit: %v", err)
	}
	tx.Rollback()

	var apiErr *APIError
	if _, err := beginRelationshipTx(context.Background(), db, "A", "D"); !errors.As(err, &apiErr) || apiErr.Status != http.StatusNotFound {
		t.Fatalf("inactive user: %v", err)
	}
}

func TestLoginSessionLocks(t *testing.T) {
	db := openLockTestDB(t)
	login := func(ctx context.Context) (*sqlx.Tx, error) {
		tx, err := db.BeginTxx(ctx, nil)
		if err != nil {
			return nil, err
		}
		var id string
		if err := tx.GetContext(ctx, &id, `SELECT id FROM users WHERE username = 'a' FOR SHARE`); err != nil {
			tx.Rollback()
			return nil, err
		}
		if _, err := createLoginSessionTx(ctx, tx, "A"); err != nil {
			tx.Rollback()
			return nil, err
		}
		return tx, nil
	}

	relationship, err := beginRelationshipTx(context.Background(), db, "A", "B")
	if err != nil {
		t.Fatal(err)
	}
	defer relationship.Rollback()
	if blocks(t, time.Second, func(ctx context.Context) error {
		tx, err := login(ctx)
		if err == nil {
			err = tx.Commit()
		}
		return err
	}) {
		t.Fatal("login waited for a relationship change of the same user")
	}
	relationship.Rollback()

	// Two logins of the same user still replace sessions one at a time.
	first, err := login(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer first.Rollback()
	if !blocks(t, 300*time.Millisecond, func(ctx context.Context) error {
		tx, err := login(ctx)
		if err == nil {
			tx.Rollback()
		}
		return err
	}) {
		t.Fatal("concurrent logins of one user were not serialized")
	}
	if err := first.Commit(); err != nil {
		t.Fatal(err)
	}
	var active int
	if err := db.Get(&active, `SELECT count(*) FROM login_sessions WHERE user_id = 'A' AND active`); err != nil || active != 1 {
		t.Fatalf("active sessions=%d err=%v", active, err)
	}
}
