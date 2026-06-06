-- Drop the vestigial `alive` flag from upstream_proxies.
--
-- `alive` was always true in practice: the sync goroutine only ever wrote
-- alive=1, stale webshare rows are DELETEd outright (not marked dead), and
-- manual rows were never toggled. Nothing in the codebase ever set it false,
-- so every `!alive` branch in routing/tunnel/swap was dead code. Removing the
-- column is a pure simplification with no behavior change. Liveness is now
-- reflected only by on-demand latency probes (last_latency_ms); the separate
-- recently_failing/recent_failure_* advisory fields are unaffected.
--
-- DROP COLUMN requires the column be unindexed first (SQLite >= 3.35).
DROP INDEX IF EXISTS idx_upstream_proxies_alive;
ALTER TABLE upstream_proxies DROP COLUMN alive;
