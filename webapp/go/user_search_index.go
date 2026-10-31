package isureal

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/jmoiron/sqlx"
)

// Unknown or stale derived values must pass the SQL prefilter. The existing Go
// NFC/fold/NFC comparison remains the final authority, including during rollback.
var searchCandidateQuery = `SELECT ` + strings.Join(userColumns, ", ") + ` FROM users
 WHERE status='active' AND id<>$1 AND ($2='' OR (username,id)>($2,$3))
 AND id NOT IN (SELECT blocked_id FROM block_relations WHERE blocker_id=$1
                UNION ALL SELECT blocker_id FROM block_relations WHERE blocked_id=$1)
 AND (search_username_source IS DISTINCT FROM username
   OR search_display_name_source IS DISTINCT FROM display_name
   OR search_username_normalized IS NULL OR search_display_name_normalized IS NULL
   OR strpos(search_username_normalized, $4)>0
   OR strpos(search_display_name_normalized, $4)>0)
 ORDER BY username, id`

// initialize restores the authoritative user fields first, then repairs this
// derived index. Matching source values guard against a concurrent profile edit.
func rebuildUserSearchIndex(ctx context.Context, db *sqlx.DB) error {
	var users []struct {
		ID                    string `db:"id" json:"id"`
		Username              string `db:"username" json:"username"`
		DisplayName           string `db:"display_name" json:"display_name"`
		UsernameNormalized    string `json:"username_normalized"`
		DisplayNameNormalized string `json:"display_name_normalized"`
	}
	if err := db.SelectContext(ctx, &users, `SELECT id, username, display_name FROM users
 WHERE search_username_source IS DISTINCT FROM username
 OR search_display_name_source IS DISTINCT FROM display_name
 OR search_username_normalized IS NULL OR search_display_name_normalized IS NULL`); err != nil {
		return err
	}
	if len(users) == 0 {
		return nil
	}
	for i := range users {
		users[i].UsernameNormalized = normalizeSearchText(users[i].Username)
		users[i].DisplayNameNormalized = normalizeSearchText(users[i].DisplayName)
	}
	data, err := json.Marshal(users)
	if err != nil {
		return err
	}
	_, err = db.ExecContext(ctx, `UPDATE users u SET
 search_username_source=v.username, search_display_name_source=v.display_name,
 search_username_normalized=v.username_normalized, search_display_name_normalized=v.display_name_normalized
 FROM jsonb_to_recordset($1::jsonb) AS v(id text, username text, display_name text, username_normalized text, display_name_normalized text)
 WHERE u.id=v.id AND u.username=v.username AND u.display_name=v.display_name`, string(data))
	return err
}
