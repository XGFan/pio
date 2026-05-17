-- Add user-controlled proxy on/off state. The daemon still binds the
-- configured ports at startup (subject to this flag); explicit Start/Stop
-- from the UI or menubar flips this and persists across restarts.

ALTER TABLE settings ADD COLUMN proxy_enabled INTEGER NOT NULL DEFAULT 1;
