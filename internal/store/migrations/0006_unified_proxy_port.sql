-- Unified single-port proxy. HTTP and SOCKS5 are now served on ONE port via
-- first-byte protocol sniffing, so the two separate listener ports collapse
-- into a single proxy_port + proxy_bind.
--
-- The old http_listener_*/socks5_listener_* columns are left in place
-- (vestigial) to avoid a destructive table rebuild; they are no longer read
-- or written. The unified port inherits the previous HTTP listener's
-- port/bind so existing deployments keep their HTTP endpoint working — only
-- the standalone SOCKS5 port (default 1080) is retired.
ALTER TABLE settings ADD COLUMN proxy_port INTEGER NOT NULL DEFAULT 8080;
ALTER TABLE settings ADD COLUMN proxy_bind TEXT    NOT NULL DEFAULT '127.0.0.1';

UPDATE settings
   SET proxy_port = http_listener_port,
       proxy_bind = http_listener_bind
 WHERE id = 1;
