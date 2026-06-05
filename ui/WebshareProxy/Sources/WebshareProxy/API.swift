// API.swift — Thin URLSession wrapper for the daemon's REST endpoints.
// All endpoints live under http://127.0.0.1:<api_port>/api/v1/. Port comes
// from the api.port file the daemon writes after binding.

import Foundation

// MARK: - DTOs

struct ApiKey: Codable, Identifiable, Hashable {
    let id: Int64
    let label: String
    let addedAt: Date
    let lastSyncedAt: Date?
    // Daemon marks this `omitempty`, so the field disappears when empty.
    private let _lastSyncError: String?
    var lastSyncError: String { _lastSyncError ?? "" }
    let active: Bool

    enum CodingKeys: String, CodingKey {
        case id, label
        case addedAt = "added_at"
        case lastSyncedAt = "last_synced_at"
        case _lastSyncError = "last_sync_error"
        case active
    }
}

struct UpstreamProxy: Codable, Identifiable, Hashable {
    let id: String
    // Defaults to "webshare" so older response shapes still decode cleanly.
    let source: String
    let sourceApiKeyId: Int64?
    let manualName: String?
    let host: String
    let port: Int
    let username: String?
    let protocol_: String
    let displayName: String
    let countryCode: String
    let cityName: String?
    let alive: Bool
    let recentlyFailing: Bool
    let lastSeenAt: Date

    var isManual: Bool { source == "manual" }

    enum CodingKeys: String, CodingKey {
        case id, source
        case sourceApiKeyId = "source_api_key_id"
        case manualName = "manual_name"
        case host, port, username
        case protocol_ = "protocol"
        case displayName = "display_name"
        case countryCode = "country_code"
        case cityName = "city_name"
        case alive
        case recentlyFailing = "recently_failing"
        case lastSeenAt = "last_seen_at"
    }

    // Custom decoder so old daemon responses (pre-manual-proxies) decode
    // cleanly: `source`, `manual_name`, `source_api_key_id`, `username`
    // may be missing or null. MUST be updated when fields are added.
    init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        self.id = try c.decode(String.self, forKey: .id)
        self.source = (try? c.decode(String.self, forKey: .source)) ?? "webshare"
        self.sourceApiKeyId = try c.decodeIfPresent(Int64.self, forKey: .sourceApiKeyId)
        self.manualName = try c.decodeIfPresent(String.self, forKey: .manualName)
        self.host = try c.decode(String.self, forKey: .host)
        self.port = try c.decode(Int.self, forKey: .port)
        self.username = try c.decodeIfPresent(String.self, forKey: .username)
        self.protocol_ = try c.decode(String.self, forKey: .protocol_)
        self.displayName = try c.decode(String.self, forKey: .displayName)
        self.countryCode = try c.decode(String.self, forKey: .countryCode)
        self.cityName = try c.decodeIfPresent(String.self, forKey: .cityName)
        self.alive = try c.decode(Bool.self, forKey: .alive)
        self.recentlyFailing = try c.decode(Bool.self, forKey: .recentlyFailing)
        self.lastSeenAt = try c.decode(Date.self, forKey: .lastSeenAt)
    }
}

struct ManualProxyInput: Codable {
    let name: String
    let host: String
    let port: Int
    let `protocol`: String
    let username: String
    let password: String
}

struct LocalUser: Codable, Identifiable, Hashable {
    var id: String { username }
    let username: String
    let upstreamProxyId: String?
    let broken: Bool
    let notes: String?
    let updatedAt: Date

    enum CodingKeys: String, CodingKey {
        case username
        case upstreamProxyId = "upstream_proxy_id"
        case broken, notes
        case updatedAt = "updated_at"
    }
}

struct Settings: Codable {
    var syncIntervalMinutes: Int
    var httpListenerPort: Int
    var httpListenerBind: String
    var socks5ListenerPort: Int
    var socks5ListenerBind: String
    var proxyEnabled: Bool
    // Read-only indicator returned by GET /api/v1/settings. Older daemons
    // that don't include this field decode to false via the custom init below.
    var universalProxyPasswordSet: Bool

    enum CodingKeys: String, CodingKey {
        case syncIntervalMinutes = "sync_interval_minutes"
        case httpListenerPort = "http_listener_port"
        case httpListenerBind = "http_listener_bind"
        case socks5ListenerPort = "socks5_listener_port"
        case socks5ListenerBind = "socks5_listener_bind"
        case proxyEnabled = "proxy_enabled"
        case universalProxyPasswordSet = "universal_proxy_password_set"
    }

    // Custom decoder so older daemons that omit `universal_proxy_password_set`
    // still decode cleanly (defaults to false).
    init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        self.syncIntervalMinutes = try c.decode(Int.self, forKey: .syncIntervalMinutes)
        self.httpListenerPort = try c.decode(Int.self, forKey: .httpListenerPort)
        self.httpListenerBind = try c.decode(String.self, forKey: .httpListenerBind)
        self.socks5ListenerPort = try c.decode(Int.self, forKey: .socks5ListenerPort)
        self.socks5ListenerBind = try c.decode(String.self, forKey: .socks5ListenerBind)
        self.proxyEnabled = try c.decode(Bool.self, forKey: .proxyEnabled)
        self.universalProxyPasswordSet = (try? c.decodeIfPresent(Bool.self, forKey: .universalProxyPasswordSet)) ?? false
    }

    init(syncIntervalMinutes: Int, httpListenerPort: Int, httpListenerBind: String,
         socks5ListenerPort: Int, socks5ListenerBind: String, proxyEnabled: Bool,
         universalProxyPasswordSet: Bool = false) {
        self.syncIntervalMinutes = syncIntervalMinutes
        self.httpListenerPort = httpListenerPort
        self.httpListenerBind = httpListenerBind
        self.socks5ListenerPort = socks5ListenerPort
        self.socks5ListenerBind = socks5ListenerBind
        self.proxyEnabled = proxyEnabled
        self.universalProxyPasswordSet = universalProxyPasswordSet
    }
}

struct ProxyStatus: Codable {
    let running: Bool
    let httpAddr: String?
    let socksAddr: String?

    enum CodingKeys: String, CodingKey {
        case running
        case httpAddr = "http_addr"
        case socksAddr = "socks_addr"
    }
}

struct ReferencingUser: Codable, Hashable {
    let username: String
    let upstreamProxyId: String
    let displayName: String
    let countryCode: String

    enum CodingKeys: String, CodingKey {
        case username
        case upstreamProxyId = "upstream_proxy_id"
        case displayName = "display_name"
        case countryCode = "country_code"
    }
}

struct KeyInUseError: Codable {
    let error: String
    let referencingUsers: [ReferencingUser]

    enum CodingKeys: String, CodingKey {
        case error
        case referencingUsers = "referencing_users"
    }
}

// MARK: - Client

enum APIError: Error, LocalizedError {
    case notReady
    case status(Int, String)
    case keyInUse([ReferencingUser])
    case upstreamInUse([ReferencingUser])
    case manualNameInUse
    case decoding(Error)
    case transport(Error)

    var errorDescription: String? {
        switch self {
        case .notReady: return "Daemon API not ready"
        case .status(let code, let body): return "HTTP \(code): \(body)"
        case .keyInUse(let refs): return "Key in use by \(refs.count) user(s)"
        case .upstreamInUse(let refs): return "Upstream in use by \(refs.count) user(s)"
        case .manualNameInUse: return "Name already used by another manual proxy"
        case .decoding(let e): return "decode: \(e)"
        case .transport(let e): return "transport: \(e)"
        }
    }
}

struct UpstreamInUseError: Codable {
    let error: String
    let referencingUsers: [ReferencingUser]

    enum CodingKeys: String, CodingKey {
        case error
        case referencingUsers = "referencing_users"
    }
}

final class APIClient {
    private let session: URLSession
    private let decoder: JSONDecoder
    private let encoder: JSONEncoder
    private(set) var baseURL: URL?

    init() {
        self.session = URLSession(configuration: .ephemeral)
        let dec = JSONDecoder()
        dec.dateDecodingStrategy = .iso8601
        self.decoder = dec
        let enc = JSONEncoder()
        enc.dateEncodingStrategy = .iso8601
        self.encoder = enc
    }

    func setPort(_ port: Int) {
        self.baseURL = URL(string: "http://127.0.0.1:\(port)")
    }

    // MARK: typed endpoints

    func listKeys() async throws -> [ApiKey] { try await getJSON("/api/v1/keys") }
    func listUpstreams() async throws -> [UpstreamProxy] { try await getJSON("/api/v1/upstreams") }
    func listUsers() async throws -> [LocalUser] { try await getJSON("/api/v1/users") }
    func getSettings() async throws -> Settings { try await getJSON("/api/v1/settings") }

    func addKey(label: String, apiKey: String) async throws {
        try await postJSON("/api/v1/keys", body: ["Label": label, "APIKey": apiKey])
    }

    func deleteKey(id: Int64) async throws {
        let (data, http) = try await rawRequest("DELETE", "/api/v1/keys/\(id)", body: nil)
        if http.statusCode == 200 { return }
        if http.statusCode == 409 {
            if let err = try? decoder.decode(KeyInUseError.self, from: data) {
                throw APIError.keyInUse(err.referencingUsers)
            }
        }
        throw APIError.status(http.statusCode, String(data: data, encoding: .utf8) ?? "")
    }

    func syncKey(id: Int64) async throws {
        try await postJSON("/api/v1/keys/\(id)/sync", body: [String: String]())
    }

    func renameUpstream(id: String, displayName: String) async throws {
        let path = "/api/v1/upstreams/\(id)"
        try await patchJSON(path, body: ["display_name": displayName])
    }

    // MARK: manual proxies

    func listManualProxies() async throws -> [UpstreamProxy] {
        try await getJSON("/api/v1/manual-proxies")
    }

    func addManualProxy(_ input: ManualProxyInput) async throws {
        let data = try encoder.encode(input)
        let (respData, http) = try await rawRequest("POST", "/api/v1/manual-proxies", body: data)
        if (200...299).contains(http.statusCode) { return }
        if http.statusCode == 409 {
            if let body = try? decoder.decode([String: String].self, from: respData),
               body["error"] == "manual_name_in_use" {
                throw APIError.manualNameInUse
            }
        }
        throw APIError.status(http.statusCode, String(data: respData, encoding: .utf8) ?? "")
    }

    func updateManualProxy(id: String, _ input: ManualProxyInput) async throws {
        let data = try encoder.encode(input)
        let (respData, http) = try await rawRequest("PATCH", "/api/v1/manual-proxies/\(id)", body: data)
        if (200...299).contains(http.statusCode) { return }
        if http.statusCode == 409 {
            if let body = try? decoder.decode([String: String].self, from: respData),
               body["error"] == "manual_name_in_use" {
                throw APIError.manualNameInUse
            }
        }
        throw APIError.status(http.statusCode, String(data: respData, encoding: .utf8) ?? "")
    }

    func deleteManualProxy(id: String) async throws {
        let (data, http) = try await rawRequest("DELETE", "/api/v1/manual-proxies/\(id)", body: nil)
        if (200...299).contains(http.statusCode) { return }
        if http.statusCode == 409 {
            if let err = try? decoder.decode(UpstreamInUseError.self, from: data) {
                throw APIError.upstreamInUse(err.referencingUsers)
            }
        }
        throw APIError.status(http.statusCode, String(data: data, encoding: .utf8) ?? "")
    }

    func addUser(username: String, password: String) async throws {
        try await postJSON("/api/v1/users", body: ["Username": username, "Password": password])
    }

    func setUserMapping(username: String, upstreamId: String?) async throws {
        try await patchJSONAny("/api/v1/users/\(username)", body: ["upstream_proxy_id": upstreamId as Any])
    }

    func deleteUser(username: String) async throws {
        let (_, http) = try await rawRequest("DELETE", "/api/v1/users/\(username)", body: nil)
        if http.statusCode != 200 { throw APIError.status(http.statusCode, "") }
    }

    func reorderUsers(_ usernames: [String]) async throws {
        let data = try JSONSerialization.data(withJSONObject: usernames)
        let (_, http) = try await rawRequest("POST", "/api/v1/users/reorder", body: data)
        guard (200...299).contains(http.statusCode) else {
            throw APIError.status(http.statusCode, "")
        }
    }

    func peekPassword(username: String) async throws -> String {
        struct PeekResponse: Codable { let username: String; let password: String }
        let resp: PeekResponse = try await getJSON("/api/v1/users/\(username)/password")
        return resp.password
    }

    func putSettings(_ s: Settings) async throws {
        try await putJSON("/api/v1/settings", body: s)
    }

    func setUniversalPassword(_ password: String) async throws {
        struct Body: Encodable { let password: String }
        try await putJSON("/api/v1/settings/universal-password", body: Body(password: password))
    }

    func proxyStatus() async throws -> ProxyStatus {
        try await getJSON("/api/v1/proxy/status")
    }

    func startProxy() async throws {
        let (data, http) = try await rawRequest("POST", "/api/v1/proxy/start", body: nil)
        if (200...299).contains(http.statusCode) { return }
        // 409 → port_in_use; surface the daemon's message verbatim.
        if let body = String(data: data, encoding: .utf8), !body.isEmpty {
            throw APIError.status(http.statusCode, body)
        }
        throw APIError.status(http.statusCode, "")
    }

    func stopProxy() async throws {
        let (_, http) = try await rawRequest("POST", "/api/v1/proxy/stop", body: nil)
        if !(200...299).contains(http.statusCode) {
            throw APIError.status(http.statusCode, "")
        }
    }

    func shutdown() async {
        _ = try? await rawRequest("POST", "/api/v1/shutdown", body: nil)
    }

    // MARK: internals

    private func getJSON<T: Decodable>(_ path: String) async throws -> T {
        let (data, http) = try await rawRequest("GET", path, body: nil)
        guard http.statusCode == 200 else { throw APIError.status(http.statusCode, String(data: data, encoding: .utf8) ?? "") }
        do { return try decoder.decode(T.self, from: data) }
        catch { throw APIError.decoding(error) }
    }

    private func postJSON<B: Encodable>(_ path: String, body: B) async throws {
        let data = try encoder.encode(body)
        let (_, http) = try await rawRequest("POST", path, body: data)
        guard (200...299).contains(http.statusCode) else {
            throw APIError.status(http.statusCode, "")
        }
    }

    private func patchJSON<B: Encodable>(_ path: String, body: B) async throws {
        let data = try encoder.encode(body)
        let (_, http) = try await rawRequest("PATCH", path, body: data)
        guard (200...299).contains(http.statusCode) else {
            throw APIError.status(http.statusCode, "")
        }
    }

    private func patchJSONAny(_ path: String, body: [String: Any?]) async throws {
        let cleaned = body.mapValues { $0 ?? NSNull() }
        let data = try JSONSerialization.data(withJSONObject: cleaned)
        let (_, http) = try await rawRequest("PATCH", path, body: data)
        guard (200...299).contains(http.statusCode) else {
            throw APIError.status(http.statusCode, "")
        }
    }

    private func putJSON<B: Encodable>(_ path: String, body: B) async throws {
        let data = try encoder.encode(body)
        let (_, http) = try await rawRequest("PUT", path, body: data)
        guard (200...299).contains(http.statusCode) else {
            throw APIError.status(http.statusCode, "")
        }
    }

    private func rawRequest(_ method: String, _ path: String, body: Data?) async throws -> (Data, HTTPURLResponse) {
        guard let base = baseURL else { throw APIError.notReady }
        let url = base.appendingPathComponent(path)
        var req = URLRequest(url: url)
        req.httpMethod = method
        req.timeoutInterval = 10
        if let body {
            req.httpBody = body
            req.setValue("application/json", forHTTPHeaderField: "Content-Type")
        }
        do {
            let (data, resp) = try await session.data(for: req)
            guard let http = resp as? HTTPURLResponse else { throw APIError.transport(URLError(.badServerResponse)) }
            return (data, http)
        } catch let apiErr as APIError {
            throw apiErr
        } catch {
            throw APIError.transport(error)
        }
    }
}
