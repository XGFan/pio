-- Per-upstream latency, measured on demand by fetching
-- http://www.gstatic.com/generate_204 through the proxy.
--   last_latency_ms: NULL = never tested, -1 = last test failed, >=0 = milliseconds.
--   last_latency_at: when the last test ran (NULL = never).
-- Display-only; routing ignores these columns.
ALTER TABLE upstream_proxies ADD COLUMN last_latency_ms INTEGER;
ALTER TABLE upstream_proxies ADD COLUMN last_latency_at DATETIME;
