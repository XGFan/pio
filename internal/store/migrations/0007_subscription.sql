-- Subscription feature. When enabled (and a universal proxy password is set),
-- the daemon serves a public GET /subscription?password=... endpoint that
-- returns a SOCKS subscription list for proxy clients. subscription_host is
-- the public ip/domain clients use to reach the unified proxy port; it is
-- substituted into each generated line's host:mixed-port authority.
ALTER TABLE settings ADD COLUMN subscription_enabled INTEGER NOT NULL DEFAULT 0;
ALTER TABLE settings ADD COLUMN subscription_host    TEXT    NOT NULL DEFAULT '';
