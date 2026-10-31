package isureal

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/jmoiron/sqlx"
)

type storedPhotoPost struct {
	Post
	Generation int64
}

// The first command locks the author. The second gets a fresh READ COMMITTED
// snapshot after that lock is acquired. It reads the author's current region,
// locks the open window, checks the cross-region daily slot and stores both rows.
// An invalid stored signing key must not allow a write followed by a 500.
const insertPhotoPostSQL = `
WITH open_slot AS MATERIALIZED (
    SELECT w.id, w.starts_at, w.on_time_deadline, slots.active, slots.deleted
    FROM users u JOIN posting_windows w ON w.region=u.region AND w.ended_at IS NULL
    CROSS JOIN LATERAL (
        SELECT count(*) FILTER (WHERE p.deleted_at IS NULL) AS active,
               count(*) FILTER (WHERE p.deleted_at IS NOT NULL) AS deleted
        FROM posts p JOIN posting_windows prior ON prior.id=p.posting_window_id
        WHERE p.author_id=u.id AND prior.local_date=w.local_date
    ) slots
    WHERE u.id=$1 AND u.status='active' AND u.secret_key_hash ~ '^[0-9A-Fa-f]{64}$'
    FOR SHARE OF w
), slot AS MATERIALIZED (
    SELECT id, starts_at, on_time_deadline, g.generation, clock_timestamp() AS posted_at
    FROM open_slot CROSS JOIN image_cache_generation g WHERE active=0 AND deleted<2
), image AS (
    INSERT INTO image_objects(id,owner_user_id,data,media_type,created_at)
    SELECT $3,$1,$10,'image/jpeg',posted_at FROM slot WHERE posted_at >= starts_at
    RETURNING id,created_at
), stored AS (
    INSERT INTO posts(id,author_id,posting_window_id,image_id,caption,visibility,
                      location_kind,latitude,longitude,locality,posted_at)
    SELECT $2,$1,slot.id,image.id,$4,$5,$6,$7,$8,$9,image.created_at FROM slot CROSS JOIN image
    RETURNING id,author_id,posting_window_id,image_id,caption,visibility,location_kind,latitude,longitude,locality,posted_at
)
SELECT s.id,s.author_id,s.posting_window_id,s.image_id,s.caption,s.visibility,
       s.location_kind,s.latitude,s.longitude,s.locality,s.posted_at,slot.on_time_deadline,slot.generation
FROM stored s CROSS JOIN slot`

func storePhotoPost(ctx context.Context, db *sqlx.DB, authorID string, input createPostRequest, location PostLocation, data []byte) (post storedPhotoPost, author User, err error) {
	conn, err := db.Conn(ctx)
	if err != nil {
		return post, author, err
	}
	defer conn.Close()
	// pgx sends both commands and the JPEG together in one implicit transaction.
	// Close waits for the database's commit before the caller warms the file or
	// returns a successful HTTP response. The normal database/sql pool is reused.
	var batch pgx.Batch
	batch.Queue("SELECT "+strings.Join(userColumns, ", ")+" FROM users WHERE id=$1 AND status='active' FOR NO KEY UPDATE", authorID)
	batch.Queue(insertPhotoPostSQL, authorID, uuid.NewString(), uuid.NewString(), input.Caption, input.Visibility,
		location.Kind, location.Latitude, location.Longitude, location.Locality, data)
	err = conn.Raw(func(driverConn any) error {
		c, ok := driverConn.(*stdlib.Conn)
		if !ok {
			return fmt.Errorf("unsupported PostgreSQL connection %T", driverConn)
		}
		results := c.Conn().SendBatch(ctx, &batch)
		authorErr := results.QueryRow().Scan(&author.ID, &author.Username, &author.SecretKeyHash, &author.DisplayName,
			&author.Bio, &author.ProfileImageID, &author.Region, &author.AccountType, &author.Status)
		postErr := results.QueryRow().Scan(&post.ID, &post.AuthorID, &post.PostingWindowID, &post.ImageID, &post.Caption,
			&post.Visibility, &post.Kind, &post.Latitude, &post.Longitude, &post.Locality, &post.PostedAt, &post.OnTimeDeadline, &post.Generation)
		if err := results.Close(); err != nil {
			return err
		}
		if authorErr != nil {
			return authorErr
		}
		if err := author.setImageKey(); err != nil {
			return err
		}
		if errors.Is(postErr, pgx.ErrNoRows) {
			return NewAPIError(http.StatusConflict, "post_slot_unavailable")
		}
		return postErr
	})
	return post, author, err
}
