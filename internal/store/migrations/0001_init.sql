-- Phase 1 schema. Future schema changes go in 0002_*.sql etc; runner applies
-- in lexicographic order and records applied filenames in schema_migrations.

CREATE TABLE api_keys (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    label           TEXT    NOT NULL,
    encrypted_key   BLOB    NOT NULL,
    added_at        DATETIME NOT NULL,
    last_synced_at  DATETIME,
    last_sync_error TEXT    NOT NULL DEFAULT '',
    active          BOOLEAN NOT NULL DEFAULT 1
);

CREATE TABLE upstream_proxies (
    id                    TEXT    PRIMARY KEY,
    source_api_key_id     INTEGER NOT NULL,
    host                  TEXT    NOT NULL,
    port                  INTEGER NOT NULL,
    username              TEXT    NOT NULL,
    encrypted_password    BLOB    NOT NULL,
    protocol              TEXT    NOT NULL DEFAULT 'http',
    display_name          TEXT    NOT NULL DEFAULT '',
    country_code          TEXT    NOT NULL DEFAULT '',
    city_name             TEXT    NOT NULL DEFAULT '',
    alive                 BOOLEAN NOT NULL DEFAULT 1,
    recently_failing      BOOLEAN NOT NULL DEFAULT 0,
    recent_failure_count  INTEGER NOT NULL DEFAULT 0,
    recent_failure_since  DATETIME,
    last_seen_at          DATETIME NOT NULL,
    -- Plan §3 FK strategy: app-level cascade, app refuses key delete via 409
    -- if any upstream is referenced from local_users (see §6). ON DELETE
    -- NO ACTION here means the DB itself blocks the delete if there are
    -- referencing rows, giving us a belt-and-suspenders guard for any path
    -- that tries to bypass the app's 409 check.
    FOREIGN KEY (source_api_key_id) REFERENCES api_keys(id) ON DELETE NO ACTION
);

CREATE INDEX idx_upstream_proxies_source_api_key_id ON upstream_proxies (source_api_key_id);
CREATE INDEX idx_upstream_proxies_country_code     ON upstream_proxies (country_code);
CREATE INDEX idx_upstream_proxies_alive            ON upstream_proxies (alive);

CREATE TABLE local_users (
    username           TEXT    PRIMARY KEY,
    password_plain     TEXT    NOT NULL,
    upstream_proxy_id  TEXT,
    broken             BOOLEAN NOT NULL DEFAULT 0,
    notes              TEXT    NOT NULL DEFAULT '',
    created_at         DATETIME NOT NULL,
    updated_at         DATETIME NOT NULL,
    -- Plan §3: SET NULL keeps the row visible so the UI can prompt
    -- "remap or delete". The app also flips broken=true in the same txn.
    FOREIGN KEY (upstream_proxy_id) REFERENCES upstream_proxies(id) ON DELETE SET NULL
);

CREATE TABLE settings (
    -- Single-row table. id=1 always.
    id                      INTEGER PRIMARY KEY CHECK (id = 1),
    sync_interval_minutes   INTEGER NOT NULL DEFAULT 60,
    http_listener_port      INTEGER NOT NULL DEFAULT 8080,
    http_listener_bind      TEXT    NOT NULL DEFAULT '127.0.0.1',
    socks5_listener_port    INTEGER NOT NULL DEFAULT 1080,
    socks5_listener_bind    TEXT    NOT NULL DEFAULT '127.0.0.1',
    api_port                INTEGER NOT NULL DEFAULT 0
);
INSERT INTO settings (id) VALUES (1);

CREATE TABLE audit_log (
    id      INTEGER PRIMARY KEY AUTOINCREMENT,
    at      DATETIME NOT NULL,
    actor   TEXT     NOT NULL,
    action  TEXT     NOT NULL,
    detail  TEXT     NOT NULL DEFAULT ''
);
CREATE INDEX idx_audit_log_at ON audit_log (at);
