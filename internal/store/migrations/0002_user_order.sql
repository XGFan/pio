-- Add user-defined ordering to local_users so the UI can drag-reorder
-- and the menubar can surface the top N entries in a stable, deterministic
-- way. Existing rows default to order_index=0; ORDER BY clauses use
-- (order_index ASC, username ASC) so unordered rows fall back to
-- alphabetical without any visual jump after the migration.

ALTER TABLE local_users ADD COLUMN order_index INTEGER NOT NULL DEFAULT 0;

CREATE INDEX IF NOT EXISTS idx_local_users_order ON local_users (order_index, username);
