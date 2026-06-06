-- Built-in "direct" upstream support.
--
-- Adds 'direct' as a third value of upstream_proxies.source so a reserved,
-- always-present row (id='direct') can represent "egress straight out of the
-- daemon's own host network" — no upstream proxy hop. The data plane dials the
-- target directly when an upstream's source is 'direct'.
--
-- SQLite can't alter a CHECK constraint in place, so we rebuild the table
-- following the same deferred-FK pattern as 0004_manual_proxies.sql. The
-- migration runner wraps each .sql in its own transaction, so PRAGMA
-- defer_foreign_keys=ON delays FK enforcement until COMMIT — at which point the
-- rebuilt table holds every old row by the same id, keeping
-- local_users.upstream_proxy_id FK-consistent.
--
-- This migration only widens the constraint; it inserts NO rows. The reserved
-- direct row is seeded idempotently at daemon boot (repo.EnsureDirectUpstream)
-- so unit-test fixtures, which apply migrations without booting the daemon,
-- keep their existing upstream counts.

PRAGMA defer_foreign_keys = ON;

CREATE TABLE upstream_proxies_new (
    id                    TEXT    PRIMARY KEY,
    source                TEXT    NOT NULL DEFAULT 'webshare'
                                  CHECK (source IN ('webshare', 'manual', 'direct')),
    source_api_key_id     INTEGER, -- NULL for manual + direct rows
    manual_name           TEXT    NOT NULL DEFAULT '',
    host                  TEXT    NOT NULL,
    port                  INTEGER NOT NULL,
    username              TEXT    NOT NULL DEFAULT '',
    encrypted_password    BLOB    NOT NULL,
    protocol              TEXT    NOT NULL DEFAULT 'http'
                                  CHECK (protocol IN ('http', 'https', 'socks5')),
    display_name          TEXT    NOT NULL DEFAULT '',
    country_code          TEXT    NOT NULL DEFAULT '',
    city_name             TEXT    NOT NULL DEFAULT '',
    recently_failing      BOOLEAN NOT NULL DEFAULT 0,
    recent_failure_count  INTEGER NOT NULL DEFAULT 0,
    recent_failure_since  DATETIME,
    last_seen_at          DATETIME NOT NULL,
    last_latency_ms       INTEGER,
    last_latency_at       DATETIME,
    FOREIGN KEY (source_api_key_id) REFERENCES api_keys(id) ON DELETE NO ACTION
);

INSERT INTO upstream_proxies_new
    (id, source, source_api_key_id, manual_name, host, port, username,
     encrypted_password, protocol, display_name, country_code, city_name,
     recently_failing, recent_failure_count, recent_failure_since,
     last_seen_at, last_latency_ms, last_latency_at)
  SELECT id, source, source_api_key_id, manual_name, host, port, username,
         encrypted_password, protocol, display_name, country_code, city_name,
         recently_failing, recent_failure_count, recent_failure_since,
         last_seen_at, last_latency_ms, last_latency_at
    FROM upstream_proxies;

DROP TABLE upstream_proxies;
ALTER TABLE upstream_proxies_new RENAME TO upstream_proxies;

CREATE INDEX idx_upstream_proxies_source_api_key_id ON upstream_proxies (source_api_key_id);
CREATE INDEX idx_upstream_proxies_country_code     ON upstream_proxies (country_code);
CREATE UNIQUE INDEX idx_upstream_proxies_manual_name
    ON upstream_proxies (manual_name) WHERE source = 'manual';
