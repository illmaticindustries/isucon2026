-- One immutable recipient snapshot per posting window. UUIDs are generated
-- once and persisted, so restarts return the same notification identities.
CREATE TABLE IF NOT EXISTS notification_batches (
  posting_window_id varchar(36) PRIMARY KEY,
  created_at timestamptz NOT NULL,
  recipients jsonb NOT NULL
);
