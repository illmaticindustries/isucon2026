package isureal

import (
	"database/sql"
	"errors"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/labstack/echo/v5"
)

type createFriendRequestRequest struct {
	RecipientID string `json:"recipientId"`
}

type createFriendRequestResponse struct {
	RelationID string    `json:"relationId" db:"relation_id"`
	Status     string    `json:"status" db:"status"`
	UpdatedAt  time.Time `json:"updatedAt" db:"updated_at"`
}

func createFriendRequestHandler(h *HandlerContext) echo.HandlerFunc {
	return func(c *echo.Context) error {
		var input createFriendRequestRequest
		if err := c.Bind(&input); err != nil {
			return err
		}
		ctx := c.Request().Context()
		actor := GetSession(c).UserID
		tx, err := beginRelationshipTx(ctx, h.DB, actor, input.RecipientID)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		// One statement gathers the blocked/friendship state and any pending
		// request, and the mutations RETURNING avoids re-reading the row, so the
		// pair lock and the user FOR SHARE are held for fewer round trips.
		var checks struct {
			Blocked          bool           `db:"blocked"`
			Friendly         bool           `db:"friendly"`
			RequestID        sql.NullString `db:"request_id"`
			RequestRequester sql.NullString `db:"request_requester"`
		}
		err = tx.GetContext(ctx, &checks, `
   SELECT
     EXISTS(SELECT 1 FROM block_relations b
       WHERE (b.blocker_id=$1 AND b.blocked_id=$2) OR (b.blocker_id=$2 AND b.blocked_id=$1)) AS blocked,
     EXISTS(SELECT 1 FROM friendships f
       WHERE f.user_low_id=$3 AND f.user_high_id=$4) AS friendly,
     pending.id AS request_id, pending.requester_id AS request_requester
   FROM (SELECT 1) AS one
   LEFT JOIN LATERAL (
     SELECT id, requester_id FROM friend_requests
     WHERE status='pending' AND
       ((requester_id=$1 AND recipient_id=$2) OR (requester_id=$2 AND recipient_id=$1))
     LIMIT 1
   ) AS pending ON true
  `, actor, input.RecipientID, min(actor, input.RecipientID), max(actor, input.RecipientID))
		if err != nil {
			return err
		}
		if checks.Blocked || checks.Friendly {
			return NewAPIError(http.StatusConflict, "relationship_conflict")
		}
		var response createFriendRequestResponse
		if checks.RequestID.Valid {
			if checks.RequestRequester.String == actor {
				return NewAPIError(http.StatusConflict, "relationship_conflict")
			}
			// The recipient requested first: accept it and create the friendship.
			if err := tx.GetContext(ctx, &response, `
   UPDATE friend_requests SET status='accepted', updated_at=clock_timestamp()
   WHERE id=$1
   RETURNING id AS relation_id, status, updated_at
  `, checks.RequestID.String); err != nil {
				return err
			}
			if err := insertFriendship(ctx, tx, FriendRequest{ID: checks.RequestID.String, RequesterID: actor, RecipientID: input.RecipientID}); err != nil {
				return err
			}
		} else {
			if err := tx.GetContext(ctx, &response, `
   INSERT INTO friend_requests (id, requester_id, recipient_id, status, created_at, updated_at)
   VALUES ($1,$2,$3,'pending',clock_timestamp(),clock_timestamp())
   RETURNING id AS relation_id, status, updated_at
  `, uuid.NewString(), actor, input.RecipientID); err != nil {
				return err
			}
		}
		if err := tx.Commit(); err != nil {
			return err
		}
		return c.JSON(http.StatusCreated, response)
	}
}

type transitionFriendRequestRequest struct {
	RequestID string `param:"requestId" json:"-"`
	Action    string `param:"action" json:"-"`
}

type transitionFriendRequestResponse createFriendRequestResponse

func transitionFriendRequestHandler(h *HandlerContext) echo.HandlerFunc {
	return func(c *echo.Context) error {
		var input transitionFriendRequestRequest
		if err := c.Bind(&input); err != nil {
			return err
		}
		var status string
		switch input.Action {
		case "accept":
			status = "accepted"
		case "reject":
			status = "rejected"
		case "cancel":
			status = "cancelled"
		default:
			return NewAPIError(http.StatusForbidden, "relationship_action_forbidden")
		}
		ctx := c.Request().Context()
		actor := GetSession(c).UserID
		request, err := getFriendRequest(ctx, h.DB, input.RequestID)
		if errors.Is(err, sql.ErrNoRows) {
			return NewAPIError(http.StatusNotFound, "relationship_not_found")
		}
		if err != nil {
			return err
		}
		if (status == "cancelled" && actor != request.RequesterID) || (status != "cancelled" && actor != request.RecipientID) {
			return NewAPIError(http.StatusForbidden, "relationship_action_forbidden")
		}
		tx, err := beginRelationshipTx(ctx, h.DB, request.RequesterID, request.RecipientID)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		request, err = getFriendRequest(ctx, tx, input.RequestID)
		if err != nil {
			return err
		}
		if request.Status != "pending" {
			return NewAPIError(http.StatusForbidden, "relationship_action_forbidden")
		}
		if status == "accepted" {
			blocked, err := isBlocked(ctx, tx, request.RequesterID, request.RecipientID)
			if err != nil {
				return err
			}
			if blocked {
				return NewAPIError(http.StatusConflict, "relationship_conflict")
			}
			if err := insertFriendship(ctx, tx, request); err != nil {
				return err
			}
		}
		if err := tx.GetContext(ctx, &request, `
   UPDATE friend_requests SET status=$2, updated_at=clock_timestamp()
   WHERE id=$1
   RETURNING id, requester_id, recipient_id, status, created_at, updated_at
  `, request.ID, status); err != nil {
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
		return c.JSON(http.StatusOK, transitionFriendRequestResponse{RelationID: request.ID, Status: request.Status, UpdatedAt: request.UpdatedAt})
	}
}
