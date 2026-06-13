-- Rename the built-in "direct" upstream to "default".
--
-- "direct" was an ambiguous user-facing name for the reserved upstream that
-- egresses straight out of the daemon's own host network (no upstream hop).
-- This migration renames that built-in's id, source tag, and display_name to
-- "default" everywhere, and keeps every local_users mapping intact.
--
-- SQLite can't alter a CHECK constraint in place, so we rebuild the table using
-- the deferred-FK pattern from 0004/0010. IMPORTANT subtlety those migrations
-- skirted: with foreign_keys=ON (our per-connection DSN), DROP TABLE performs an
-- implicit row-delete that FIRES local_users' `ON DELETE SET NULL` action and
-- would null out every non-NULL user→upstream mapping. defer_foreign_keys delays constraint
-- *violation checks* until COMMIT, NOT the SET NULL *action*. So we snapshot the
-- mappings (remapping the renamed built-in id) into a temp table before the
-- rebuild and restore them afterward.
--
-- Existing deployments may hold the legacy row (id/source/display_name all
-- 'direct') and users mapped to it; both are migrated here. A fresh database has
-- no such row yet (it is seeded at boot by repo.EnsureDefaultUpstream), so the
-- remap CASEs and the snapshot/restore simply affect zero rows.

PRAGMA defer_foreign_keys = ON;

-- 1. Snapshot user→upstream mappings, remapping the renamed built-in.
CREATE TEMP TABLE _lu_map AS
  SELECT username,
         CASE WHEN upstream_proxy_id = 'direct' THEN 'default'
              ELSE upstream_proxy_id END AS upstream_proxy_id
    FROM local_users
   WHERE upstream_proxy_id IS NOT NULL;

-- 2. Rebuild upstream_proxies with the new CHECK, remapping the built-in row.
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
  SELECT
         CASE WHEN id = 'direct' THEN 'default' ELSE id END,
         CASE WHEN source = 'direct' THEN 'default' ELSE source END,
         source_api_key_id, manual_name, host, port, username,
         encrypted_password, protocol,
         CASE WHEN source = 'direct' AND display_name = 'direct' THEN 'default'
              ELSE display_name END,
         country_code, city_name,
         recently_failing, recent_failure_count, recent_failure_since,
         last_seen_at, last_latency_ms, last_latency_at
    FROM upstream_proxies;

DROP TABLE upstream_proxies;
ALTER TABLE upstream_proxies_new RENAME TO upstream_proxies;

-- 3. Restore the mappings the DROP cleared (via ON DELETE SET NULL).
UPDATE local_users
   SET upstream_proxy_id = (SELECT m.upstream_proxy_id FROM _lu_map m
                             WHERE m.username = local_users.username)
 WHERE username IN (SELECT username FROM _lu_map);

DROP TABLE _lu_map;

CREATE INDEX idx_upstream_proxies_source_api_key_id ON upstream_proxies (source_api_key_id);
CREATE INDEX idx_upstream_proxies_country_code     ON upstream_proxies (country_code);
CREATE UNIQUE INDEX idx_upstream_proxies_manual_name
    ON upstream_proxies (manual_name) WHERE source = 'manual';
