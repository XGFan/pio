-- Universal proxy password (US: display-name routing). When set, a client
-- can authenticate with (display_name, universal_proxy_password) and is
-- routed to the upstream whose display_name matches — without needing a
-- dedicated local_users row per proxy.
--
-- Stored AES-256-GCM-encrypted (AAD = "settings.universal_proxy_password_enc")
-- because it is a high-value master credential that grants access to every
-- proxy. A zero-length blob (X'') means "unset / feature disabled".
ALTER TABLE settings ADD COLUMN universal_proxy_password_enc BLOB NOT NULL DEFAULT X'';
