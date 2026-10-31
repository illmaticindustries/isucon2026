package isureal

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"testing"
)

func TestConcurrentSessionReplacement(t *testing.T) {
	db := isolatedWriteTestDB(t)
	schema, err := os.ReadFile("../sql/001-schema.sql")
	if err != nil {
		t.Fatal(err)
	}
	db.MustExec(string(schema))
	db.MustExec(`INSERT INTO users(id,username,secret_key_hash,display_name,region) VALUES('author','author',repeat('12',32),'Author','asia')`)
	type result struct {
		session *Session
		err     error
	}
	results := make(chan result, 2)
	for range 2 {
		go func() { sess, err := CreateLoginSession(db, "author"); results <- result{sess, err} }()
	}
	for range 2 {
		r := <-results
		if r.err != nil {
			t.Fatal(r.err)
		}
		hash := sha256.Sum256([]byte(r.session.Token))
		if r.session.UserID != "author" || r.session.CreatedAt.IsZero() || r.session.TokenHash != hex.EncodeToString(hash[:]) {
			t.Fatal("session mismatch")
		}
	}
	var counts struct {
		Active   int
		Replaced int
	}
	if err := db.Get(&counts, `SELECT count(*) FILTER (WHERE active) AS active, count(*) FILTER (WHERE NOT active AND reason='replaced' AND invalidated_at IS NOT NULL) AS replaced FROM login_sessions WHERE user_id='author'`); err != nil {
		t.Fatal(err)
	}
	if counts.Active != 1 || counts.Replaced != 1 {
		t.Fatalf("unexpected session counts: %+v", counts)
	}
}
