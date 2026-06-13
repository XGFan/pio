-- Retire the built-in "direct" upstream in favour of "default".
--
-- "direct" was an ambiguous user-facing name for the reserved upstream that
-- egresses straight out of the daemon's own host network. This migration drops
-- the legacy row outright and widens the source CHECK to "default"; the new
-- built-in is seeded fresh at boot (repo.EnsureDefaultUpstream). No state is
-- carried over from "direct" — a clean rename, no compatibility shim.
--
-- SQLite can't alter a CHECK constraint in place, so we rebuild the table using
-- the deferred-FK pattern from 0004/0010. IMPORTANT subtlety: with foreign_keys
-- ON (our per-connection DSN), DROP TABLE performs an implicit row-delete that
-- FIRES local_users' `ON DELETE SET NULL` action and would null out every
-- non-NULL user→upstream mapping. defer_foreign_keys delays constraint
-- *violation checks* until COMMIT, NOT the SET NULL *action*. So we snapshot the
-- non-"direct" mappings before the rebuild and restore them after. Any user that
-- was mapped to "direct" is intentionally left unmapped (it was never offered in
-- the UI, so there should be none).
--
-- On a fresh database there is no "direct" row, so the rebuild simply swaps the
-- CHECK and the snapshot/restore is a no-op.

PRAGMA defer_foreign_keys = ON;

-- 1. Snapshot user→upstream mappings (excluding the retired "direct").
CREATE TEMP TABLE _lu_map AS
  SELECT username, upstream_proxy_id
    FROM local_users
   WHERE upstream_proxy_id IS NOT NULL
     AND upstream_proxy_id <> 'direct';

-- 2. Rebuild upstream_proxies with the new CHECK, dropping the "direct" row.
CREATE TABLE upstream_proxies_new (
    id                    TEXT    PRIMARY KEY,
    source                TEXT    NOT NULL DEFAULT 'webshare'
                                  CHECK (source IN ('webshare', 'manual', 'default')),
    source_api_key_id     INTEGER, -- NULL for manual + default rows
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
    FROM upstream_proxies
   WHERE source <> 'direct';

DROP TABLE upstream_proxies;
ALTER TABLE upstream_proxies_new RENAME TO upstream_proxies;

-- 3. Restore the snapshotted mappings the DROP cleared (via ON DELETE SET NULL).
UPDATE local_users
   SET upstream_proxy_id = (SELECT m.upstream_proxy_id FROM _lu_map m
                             WHERE m.username = local_users.username)
 WHERE username IN (SELECT username FROM _lu_map);

DROP TABLE _lu_map;

CREATE INDEX idx_upstream_proxies_source_api_key_id ON upstream_proxies (source_api_key_id);
CREATE INDEX idx_upstream_proxies_country_code     ON upstream_proxies (country_code);
CREATE UNIQUE INDEX idx_upstream_proxies_manual_name
    ON upstream_proxies (manual_name) WHERE source = 'manual';
