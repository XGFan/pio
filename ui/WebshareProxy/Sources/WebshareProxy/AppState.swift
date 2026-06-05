// AppState.swift — Observable app state: holds the API client, current
// snapshots of keys/upstreams/users/settings, and reload helpers.

import SwiftUI

@MainActor
final class AppState: ObservableObject {
    static let shared = AppState()

    let api = APIClient()
    let supervisor = DaemonSupervisor()

    @Published var ready: Bool = false
    @Published var statusMessage: String = "Starting daemon…"

    @Published var keys: [ApiKey] = []
    @Published var upstreams: [UpstreamProxy] = []
    @Published var users: [LocalUser] = []
    @Published var settings: Settings = Settings(
        syncIntervalMinutes: 60,
        httpListenerPort: 8080, httpListenerBind: "127.0.0.1",
        socks5ListenerPort: 1080, socks5ListenerBind: "127.0.0.1",
        proxyEnabled: true,
        universalProxyPasswordSet: false
    )
    @Published var lastError: String?

    // Proxy lifecycle — surfaced to the UI so the Start/Stop control reflects
    // daemon-truth and so a port-conflict error can be shown inline (not as
    // a dialog).
    @Published var proxyRunning: Bool = false
    @Published var proxyHTTPAddr: String = ""
    @Published var proxySocksAddr: String = ""
    @Published var listenerError: String?

    func bootstrap() async {
        do {
            try supervisor.start()
            let port = try supervisor.waitForAPIPort()
            api.setPort(port)
            statusMessage = "Connected to daemon on 127.0.0.1:\(port)"
            ready = true
            DaemonSupervisor.log("bootstrap success port=\(port)")
            await refreshAll()
        } catch {
            statusMessage = "Failed: \(error.localizedDescription)"
            lastError = error.localizedDescription
            DaemonSupervisor.log("bootstrap failed: \(error.localizedDescription)")
        }
    }

    func refreshAll() async {
        async let k = (try? api.listKeys()) ?? []
        async let u = (try? api.listUpstreams()) ?? []
        async let usr = (try? api.listUsers()) ?? []
        async let s = (try? api.getSettings())
        async let p = try? api.proxyStatus()
        self.keys = await k
        // listUpstreams already returns both webshare and manual rows; the
        // manual subset is derived for the dedicated Manual Proxies section.
        self.upstreams = await u
        self.users = await usr
        if let s = await s { self.settings = s }
        if let p = await p {
            self.proxyRunning = p.running
            self.proxyHTTPAddr = p.httpAddr ?? ""
            self.proxySocksAddr = p.socksAddr ?? ""
        }
    }

    var manualProxies: [UpstreamProxy] {
        upstreams.filter { $0.isManual }
    }

    // Start the proxy listeners. Returns true on success; on failure the
    // daemon's message is stashed in listenerError for the UI to render.
    @discardableResult
    func startProxy() async -> Bool {
        do {
            try await api.startProxy()
            listenerError = nil
            await refreshAll()
            return true
        } catch {
            listenerError = error.localizedDescription
            await refreshAll()
            return false
        }
    }

    @discardableResult
    func stopProxy() async -> Bool {
        do {
            try await api.stopProxy()
            listenerError = nil
            await refreshAll()
            return true
        } catch {
            listenerError = error.localizedDescription
            return false
        }
    }

    // applySettings is the System-section "Apply" path. Surfaces a port-in-use
    // conflict inline so the old listener stays bound (req #3 — 旧端口不释放).
    @discardableResult
    func applySettings(_ s: Settings) async -> Bool {
        do {
            try await api.putSettings(s)
            listenerError = nil
            await refreshAll()
            return true
        } catch {
            listenerError = error.localizedDescription
            await refreshAll()
            return false
        }
    }

    @discardableResult
    func setUniversalPassword(_ password: String) async -> Bool {
        do {
            try await api.setUniversalPassword(password)
            listenerError = nil
            await refreshAll()
            return true
        } catch {
            listenerError = error.localizedDescription
            await refreshAll()
            return false
        }
    }

    func shutdown() async {
        await supervisor.shutdown(api: api)
    }
}
