-- Phase 5: support manually-added upstream proxies alongside webshare-sync'd
-- ones. Adds a `source` discriminator + a `manual_name` field (unique among
-- manual rows) and makes `source_api_key_id` nullable so manual rows can
-- exist without an api_keys row.
--
-- SQLite can't drop a NOT NULL / FK constraint in place, so we follow the
-- canonical rebuild pattern from https://sqlite.org/lang_altertable.html.
-- The migration runner wraps each .sql in its own transaction, so we use
-- `PRAGMA defer_foreign_keys=ON` (works inside a transaction, unlike
-- `foreign_keys=OFF` which doesn't) to delay FK enforcement until COMMIT.
-- At commit time the new upstream_proxies table contains every old row by
-- the same id, so local_users.upstream_proxy_id remains FK-consistent.

PRAGMA defer_foreign_keys = ON;

CREATE TABLE upstream_proxies_new (
    id                    TEXT    PRIMARY KEY,
    source                TEXT    NOT NULL DEFAULT 'webshare'
                                  CHECK (source IN ('webshare', 'manual')),
    source_api_key_id     INTEGER, -- NULL for manual rows
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
    alive                 BOOLEAN NOT NULL DEFAULT 1,
    recently_failing      BOOLEAN NOT NULL DEFAULT 0,
    recent_failure_count  INTEGER NOT NULL DEFAULT 0,
    recent_failure_since  DATETIME,
    last_seen_at          DATETIME NOT NULL,
    FOREIGN KEY (source_api_key_id) REFERENCES api_keys(id) ON DELETE NO ACTION
);

INSERT INTO upstream_proxies_new
    (id, source, source_api_key_id, manual_name, host, port, username,
     encrypted_password, protocol, display_name, country_code, city_name,
     alive, recently_failing, recent_failure_count, recent_failure_since,
     last_seen_at)
  SELECT id, 'webshare', source_api_key_id, '', host, port, username,
         encrypted_password, protocol, display_name, country_code, city_name,
         alive, recently_failing, recent_failure_count, recent_failure_since,
         last_seen_at
    FROM upstream_proxies;

DROP TABLE upstream_proxies;
ALTER TABLE upstream_proxies_new RENAME TO upstream_proxies;

CREATE INDEX idx_upstream_proxies_source_api_key_id ON upstream_proxies (source_api_key_id);
CREATE INDEX idx_upstream_proxies_country_code     ON upstream_proxies (country_code);
CREATE INDEX idx_upstream_proxies_alive            ON upstream_proxies (alive);
CREATE UNIQUE INDEX idx_upstream_proxies_manual_name
    ON upstream_proxies (manual_name) WHERE source = 'manual';
