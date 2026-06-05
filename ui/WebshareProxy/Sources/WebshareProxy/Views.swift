// Views.swift — Main window with two tabs (Proxy Sources / Users & Rules)
// plus the Add API Key and Add User modals.

import SwiftUI

struct MainWindow: View {
    @EnvironmentObject var state: AppState
    @State private var tab: Tab = .sources

    enum Tab: String, CaseIterable { case sources = "Proxy Sources"; case users = "Users & Rules" }

    var body: some View {
        VStack(spacing: 0) {
            Picker("", selection: $tab) {
                ForEach(Tab.allCases, id: \.self) { Text($0.rawValue).tag($0) }
            }
            .pickerStyle(.segmented)
            .padding(.horizontal, 12).padding(.vertical, 8)

            Divider()

            Group {
                switch tab {
                case .sources: ProxySourcesView()
                case .users: UsersRulesView()
                }
            }
            .frame(maxWidth: .infinity, maxHeight: .infinity)
        }
        .frame(minWidth: 720, minHeight: 480)
        .task { await state.refreshAll() }
    }
}

// MARK: - Proxy Sources tab

struct ProxySourcesView: View {
    @EnvironmentObject var state: AppState
    @State private var universalPasswordInput: String = ""
    @State private var showAddKey = false
    @State private var deleteConflict: [ReferencingUser]? = nil
    @State private var manualDeleteConflict: [ReferencingUser]? = nil
    @State private var manualEditing: UpstreamProxy? = nil
    @State private var showAddManual = false

    var body: some View {
        ScrollView {
            VStack(alignment: .leading, spacing: 16) {
                systemSection
                Divider()
                webshareSection
                Divider()
                manualProxiesSection
            }
            .padding(16)
        }
        .sheet(isPresented: $showAddKey) { AddAPIKeyModal().environmentObject(state) }
        .sheet(isPresented: $showAddManual) {
            ManualProxyEditor(existing: nil).environmentObject(state)
        }
        .sheet(item: $manualEditing) { mp in
            ManualProxyEditor(existing: mp).environmentObject(state)
        }
        .alert("Key is in use", isPresented: .constant(deleteConflict != nil)) {
            Button("OK") { deleteConflict = nil }
        } message: {
            if let conflict = deleteConflict {
                Text(conflict.map { "• \($0.username) → \($0.displayName)" }.joined(separator: "\n"))
            }
        }
        .alert("Manual proxy is in use", isPresented: .constant(manualDeleteConflict != nil)) {
            Button("OK") { manualDeleteConflict = nil }
        } message: {
            if let conflict = manualDeleteConflict {
                Text("Remap or delete these users first:\n" +
                     conflict.map { "• \($0.username)" }.joined(separator: "\n"))
            }
        }
    }

    private var systemSection: some View {
        VStack(alignment: .leading, spacing: 8) {
            HStack(spacing: 8) {
                Text("System").font(.headline)
                proxyStatusBadge
                Spacer()
                proxyToggleButton
            }
            HStack(spacing: 16) {
                HStack {
                    Text("Listen addr:").font(.subheadline)
                    Picker("", selection: sharedBind) {
                        Text("127.0.0.1").tag("127.0.0.1")
                        Text("0.0.0.0").tag("0.0.0.0")
                        Text("[::1]").tag("[::1]")
                    }
                    .frame(width: 120).labelsHidden()
                }
                HStack {
                    Text("Proxy port:").font(.subheadline)
                    TextField("Port", value: $state.settings.proxyPort, format: .number.grouping(.never))
                        .frame(width: 70)
                }
                HStack {
                    Text("Sync (min):").font(.subheadline)
                    TextField("", value: $state.settings.syncIntervalMinutes, format: .number.grouping(.never))
                        .frame(width: 60)
                }
                Button("Apply") {
                    Task { await state.applySettings(state.settings) }
                }
            }
            HStack(spacing: 8) {
                Text("Universal password:").font(.subheadline)
                SecureField("New password", text: $universalPasswordInput)
                    .frame(width: 160)
                Button("Save") {
                    Task {
                        _ = await state.setUniversalPassword(universalPasswordInput)
                        universalPasswordInput = ""
                    }
                }
                if state.settings.universalProxyPasswordSet {
                    Button("Clear") {
                        Task { _ = await state.setUniversalPassword("") }
                    }
                    .foregroundStyle(.red)
                }
                Text(state.settings.universalProxyPasswordSet ? "Set" : "Not set")
                    .font(.caption)
                    .foregroundStyle(state.settings.universalProxyPasswordSet ? .green : .secondary)
            }
            if let err = state.listenerError {
                Text(err)
                    .foregroundStyle(.red)
                    .font(.caption)
                    .lineLimit(2)
            }
        }
    }

    private var proxyStatusBadge: some View {
        HStack(spacing: 4) {
            Circle()
                .fill(state.proxyRunning ? Color.green : Color.gray)
                .frame(width: 8, height: 8)
            Text(state.proxyRunning ? "Running" : "Stopped")
                .font(.caption)
                .foregroundStyle(.secondary)
        }
    }

    private var proxyToggleButton: some View {
        Button(state.proxyRunning ? "Stop Proxy" : "Start Proxy") {
            Task {
                if state.proxyRunning {
                    await state.stopProxy()
                } else {
                    await state.startProxy()
                }
            }
        }
    }

    private var sharedBind: Binding<String> {
        Binding(
            get: { state.settings.proxyBind },
            set: { newValue in
                state.settings.proxyBind = newValue
            }
        )
    }

    private var webshareSection: some View {
        VStack(alignment: .leading, spacing: 12) {
            HStack {
                Text("Webshare").font(.headline)
                Spacer()
                Button(action: { showAddKey = true }) { Image(systemName: "plus.circle") }
                    .buttonStyle(.borderless)
                Button(action: { Task { await state.refreshAll() } }) { Image(systemName: "arrow.clockwise") }
                    .buttonStyle(.borderless)
            }
            if state.keys.isEmpty {
                Text("No API keys configured. Click + to add one.")
                    .foregroundStyle(.secondary).font(.subheadline)
            } else {
                ForEach(state.keys) { key in keyCard(key) }
            }
        }
    }

    private func keyCard(_ key: ApiKey) -> some View {
        let owned = state.upstreams.filter { $0.sourceApiKeyId == key.id }
        return VStack(alignment: .leading, spacing: 4) {
            HStack {
                Text(key.label).font(.headline)
                Spacer()
                if !key.lastSyncError.isEmpty {
                    Image(systemName: "circle.fill").foregroundStyle(.red).help(key.lastSyncError)
                }
                if let t = key.lastSyncedAt {
                    Text(t.formatted(.relative(presentation: .numeric))).font(.caption).foregroundStyle(.secondary)
                }
                Button(action: { Task { try? await state.api.syncKey(id: key.id); await state.refreshAll() } }) {
                    Image(systemName: "arrow.clockwise")
                }.buttonStyle(.borderless)
                Button(action: { Task { await deleteKey(id: key.id) } }) { Image(systemName: "trash") }
                    .buttonStyle(.borderless).foregroundStyle(.red)
            }
            if owned.isEmpty {
                Text("No upstreams synced yet").font(.caption).foregroundStyle(.secondary)
            } else {
                upstreamList(owned)
            }
        }
        .padding(8)
        .background(RoundedRectangle(cornerRadius: 6).fill(Color.secondary.opacity(0.06)))
    }

    // Inline upstream list — replaces the per-key Table so the row count is
    // not bounded by a scrolling subview. The outer ScrollView remains the
    // only scroller.
    private func upstreamList(_ rows: [UpstreamProxy]) -> some View {
        VStack(alignment: .leading, spacing: 0) {
            HStack(spacing: 8) {
                Text("Country").frame(width: 60, alignment: .leading)
                Text("Display Name").frame(maxWidth: .infinity, alignment: .leading)
                Text("Node Address").frame(maxWidth: .infinity, alignment: .leading)
                Text("Alive").frame(width: 50, alignment: .trailing)
            }
            .font(.caption).foregroundStyle(.secondary).padding(.vertical, 4)
            Divider()
            ForEach(rows) { up in
                HStack(spacing: 8) {
                    Text(up.countryCode).frame(width: 60, alignment: .leading)
                    Text(up.displayName).frame(maxWidth: .infinity, alignment: .leading)
                    Text(up.host + ":" + String(up.port))
                        .font(.system(.body, design: .monospaced))
                        .frame(maxWidth: .infinity, alignment: .leading)
                    Image(systemName: up.alive ? "checkmark.circle.fill" : "xmark.circle.fill")
                        .foregroundStyle(up.alive ? .green : .red)
                        .frame(width: 50, alignment: .trailing)
                }
                .padding(.vertical, 2)
            }
        }
    }

    private func deleteKey(id: Int64) async {
        do {
            try await state.api.deleteKey(id: id)
            await state.refreshAll()
        } catch let APIError.keyInUse(refs) {
            deleteConflict = refs
        } catch {
            // ignore — UI shows nothing for transient errors here
        }
    }

    // MARK: Manual proxies

    private var manualProxiesSection: some View {
        VStack(alignment: .leading, spacing: 12) {
            HStack {
                Text("Manual Proxies").font(.headline)
                Spacer()
                Button(action: { showAddManual = true }) { Image(systemName: "plus.circle") }
                    .buttonStyle(.borderless)
            }
            if state.manualProxies.isEmpty {
                Text("No manual proxies. Click + to add one.")
                    .foregroundStyle(.secondary).font(.subheadline)
            } else {
                VStack(alignment: .leading, spacing: 0) {
                    HStack(spacing: 8) {
                        Text("Name").frame(width: 140, alignment: .leading)
                        Text("Host:Port").frame(maxWidth: .infinity, alignment: .leading)
                        Text("Protocol").frame(width: 70, alignment: .leading)
                        Text("Username").frame(width: 120, alignment: .leading)
                        Text("").frame(width: 60)
                    }
                    .font(.caption).foregroundStyle(.secondary).padding(.vertical, 4)
                    Divider()
                    ForEach(state.manualProxies) { mp in
                        HStack(spacing: 8) {
                            Text(mp.manualName ?? mp.displayName)
                                .frame(width: 140, alignment: .leading)
                            Text("\(mp.host):\(mp.port)")
                                .font(.system(.body, design: .monospaced))
                                .frame(maxWidth: .infinity, alignment: .leading)
                            Text(mp.protocol_.uppercased())
                                .font(.caption)
                                .padding(.horizontal, 6).padding(.vertical, 2)
                                .background(RoundedRectangle(cornerRadius: 4).fill(Color.secondary.opacity(0.15)))
                                .frame(width: 70, alignment: .leading)
                            Text(mp.username ?? "")
                                .foregroundStyle((mp.username ?? "").isEmpty ? .secondary : .primary)
                                .frame(width: 120, alignment: .leading)
                            HStack(spacing: 4) {
                                Button(action: { manualEditing = mp }) { Image(systemName: "pencil") }
                                    .buttonStyle(.borderless)
                                Button(action: { Task { await deleteManualProxy(mp) } }) {
                                    Image(systemName: "trash")
                                }
                                .buttonStyle(.borderless).foregroundStyle(.red)
                            }
                            .frame(width: 60, alignment: .trailing)
                        }
                        .padding(.vertical, 2)
                    }
                }
                .padding(8)
                .background(RoundedRectangle(cornerRadius: 6).fill(Color.secondary.opacity(0.06)))
            }
        }
    }

    private func deleteManualProxy(_ mp: UpstreamProxy) async {
        do {
            try await state.api.deleteManualProxy(id: mp.id)
            await state.refreshAll()
        } catch let APIError.upstreamInUse(refs) {
            manualDeleteConflict = refs
        } catch {
            // ignored
        }
    }
}

// MARK: - Users & Rules tab

struct UsersRulesView: View {
    @EnvironmentObject var state: AppState
    @State private var showAddUser = false
    @State private var revealedPasswords: [String: String] = [:]

    var body: some View {
        VStack(alignment: .leading, spacing: 8) {
            HStack {
                Text("Users").font(.headline)
                Text("Drag to reorder — top 3 appear in the menubar").font(.caption).foregroundStyle(.secondary)
                Spacer()
                Button(action: { showAddUser = true }) { Image(systemName: "plus.circle") }
                    .buttonStyle(.borderless)
            }.padding(.horizontal, 16).padding(.top, 12)

            HStack(spacing: 8) {
                Text("Username").frame(width: 140, alignment: .leading)
                Text("Mapped Proxy").frame(width: 230, alignment: .leading)
                Text("Password").frame(width: 160, alignment: .leading)
                Text("Status").frame(width: 60, alignment: .leading)
                Spacer()
            }
            .font(.caption).foregroundStyle(.secondary)
            .padding(.horizontal, 16)

            List {
                ForEach(state.users) { user in
                    userRow(user)
                }
                .onMove(perform: moveUsers)
            }
            .listStyle(.plain)
            .frame(maxHeight: .infinity)
        }
        .sheet(isPresented: $showAddUser) { AddUserModal().environmentObject(state) }
    }

    private func userRow(_ user: LocalUser) -> some View {
        HStack(spacing: 8) {
            Text(user.username).frame(width: 140, alignment: .leading)
            Picker("", selection: bindingForMapping(user)) {
                Text("— (unmapped)").tag(Optional<String>.none)
                ForEach(state.upstreams) { u in
                    Text(u.isManual ? "\(u.displayName) (manual)" : u.displayName).tag(Optional(u.id))
                }
            }
            .labelsHidden()
            .frame(width: 230, alignment: .leading)
            HStack {
                if let p = revealedPasswords[user.username] {
                    Text(p).font(.system(.caption, design: .monospaced))
                }
                Button(action: { Task { await peek(user.username) } }) {
                    Image(systemName: revealedPasswords[user.username] == nil ? "eye" : "eye.slash")
                }.buttonStyle(.borderless)
            }.frame(width: 160, alignment: .leading)
            if user.broken {
                Image(systemName: "exclamationmark.triangle.fill")
                    .foregroundStyle(.red)
                    .help("Mapping broken — upstream missing or stale")
                    .frame(width: 60, alignment: .leading)
            } else {
                Image(systemName: "checkmark.circle.fill")
                    .foregroundStyle(.green)
                    .frame(width: 60, alignment: .leading)
            }
            Spacer()
            Button(action: { Task { try? await state.api.deleteUser(username: user.username); await state.refreshAll() } }) {
                Image(systemName: "trash")
            }.buttonStyle(.borderless).foregroundStyle(.red)
        }
    }

    private func moveUsers(from source: IndexSet, to destination: Int) {
        var reordered = state.users
        reordered.move(fromOffsets: source, toOffset: destination)
        state.users = reordered
        let usernames = reordered.map(\.username)
        Task {
            try? await state.api.reorderUsers(usernames)
            await state.refreshAll()
        }
    }

    private func bindingForMapping(_ user: LocalUser) -> Binding<String?> {
        Binding(
            get: { user.upstreamProxyId },
            set: { newID in
                Task {
                    try? await state.api.setUserMapping(username: user.username, upstreamId: newID)
                    await state.refreshAll()
                }
            }
        )
    }

    private func peek(_ username: String) async {
        if revealedPasswords[username] != nil {
            revealedPasswords[username] = nil
            return
        }
        if let p = try? await state.api.peekPassword(username: username) {
            revealedPasswords[username] = p
            // Auto-hide after 5s.
            Task { @MainActor in
                try? await Task.sleep(nanoseconds: 5_000_000_000)
                revealedPasswords[username] = nil
            }
        }
    }
}

// MARK: - Modals

struct AddAPIKeyModal: View {
    @Environment(\.dismiss) var dismiss
    @EnvironmentObject var state: AppState
    @State private var label = ""
    @State private var key = ""
    @State private var errorText: String?
    @State private var submitting = false

    var body: some View {
        VStack(alignment: .leading, spacing: 12) {
            Text("Add API Key").font(.headline)
            TextField("Label (e.g. US Premium)", text: $label)
            SecureField("API Key (sk_...)", text: $key)
            if let errorText {
                Text(errorText).foregroundStyle(.red).font(.caption)
            }
            HStack {
                Spacer()
                Button("Cancel") { dismiss() }
                Button(submitting ? "Adding…" : "Add") {
                    submit()
                }
                .keyboardShortcut(.return)
                .disabled(label.isEmpty || key.isEmpty || submitting)
            }
        }.padding(20).frame(width: 360)
    }

    private func submit() {
        submitting = true
        errorText = nil
        Task {
            do {
                try await state.api.addKey(label: label, apiKey: key)
                await state.refreshAll()
                dismiss()
            } catch {
                errorText = error.localizedDescription
                submitting = false
            }
        }
    }
}

struct AddUserModal: View {
    @Environment(\.dismiss) var dismiss
    @EnvironmentObject var state: AppState
    @State private var username = ""
    @State private var password = ""
    @State private var initialUpstreamID: String? = nil
    @State private var errorText: String?
    @State private var submitting = false

    var body: some View {
        VStack(alignment: .leading, spacing: 12) {
            Text("Add User").font(.headline)
            TextField("Username", text: $username)
            SecureField("Password", text: $password)
            HStack {
                Text("Proxy:").font(.subheadline)
                Picker("", selection: $initialUpstreamID) {
                    Text("— (no mapping)").tag(Optional<String>.none)
                    ForEach(state.upstreams) { up in
                        Text(up.isManual ? "\(up.displayName) (manual)" : up.displayName).tag(Optional(up.id))
                    }
                }
                .labelsHidden()
            }
            if let errorText {
                Text(errorText).foregroundStyle(.red).font(.caption)
            }
            HStack {
                Spacer()
                Button("Cancel") { dismiss() }
                Button(submitting ? "Adding…" : "Add") {
                    submit()
                }
                .keyboardShortcut(.return)
                .disabled(username.isEmpty || password.isEmpty || submitting)
            }
        }.padding(20).frame(width: 380)
    }

    private func submit() {
        submitting = true
        errorText = nil
        Task {
            do {
                try await state.api.addUser(username: username, password: password)
                if let upstreamID = initialUpstreamID {
                    try await state.api.setUserMapping(username: username, upstreamId: upstreamID)
                }
                await state.refreshAll()
                dismiss()
            } catch {
                errorText = error.localizedDescription
                submitting = false
            }
        }
    }
}

// ManualProxyEditor doubles as add + edit. `existing == nil` → add mode;
// otherwise edit mode and the password field defaults to blank meaning
// "leave existing password unchanged" (the server preserves the ciphertext
// when password is empty).
struct ManualProxyEditor: View {
    @Environment(\.dismiss) var dismiss
    @EnvironmentObject var state: AppState
    let existing: UpstreamProxy?

    @State private var name = ""
    @State private var host = ""
    @State private var port = "8080"
    @State private var proto = "http"
    @State private var username = ""
    @State private var password = ""
    @State private var errorText: String?
    @State private var submitting = false

    var isEdit: Bool { existing != nil }

    var body: some View {
        VStack(alignment: .leading, spacing: 12) {
            Text(isEdit ? "Edit Manual Proxy" : "Add Manual Proxy").font(.headline)
            TextField("Name (unique)", text: $name)
            TextField("Host", text: $host)
            HStack {
                TextField("Port", text: $port).frame(width: 100)
                Picker("Protocol", selection: $proto) {
                    Text("HTTP").tag("http")
                    Text("HTTPS").tag("https")
                    Text("SOCKS5").tag("socks5")
                }
            }
            TextField("Username (optional)", text: $username)
            SecureField(isEdit ? "Password (leave blank to keep current)" : "Password (optional)", text: $password)
            if let errorText {
                Text(errorText).foregroundStyle(.red).font(.caption)
            }
            HStack {
                Spacer()
                Button("Cancel") { dismiss() }
                Button(submitting ? "Saving…" : (isEdit ? "Save" : "Add")) { submit() }
                    .keyboardShortcut(.return)
                    .disabled(submitting || name.isEmpty || host.isEmpty || (Int(port) ?? 0) <= 0)
            }
        }
        .padding(20)
        .frame(width: 420)
        .onAppear(perform: hydrateFromExisting)
    }

    private func hydrateFromExisting() {
        guard let mp = existing else { return }
        name = mp.manualName ?? mp.displayName
        host = mp.host
        port = String(mp.port)
        proto = mp.protocol_
        username = mp.username ?? ""
        // password stays blank — empty means "keep".
    }

    private func submit() {
        guard let p = Int(port), p > 0, p <= 65535 else {
            errorText = "Port must be 1–65535"
            return
        }
        submitting = true
        errorText = nil
        let input = ManualProxyInput(
            name: name, host: host, port: p, protocol: proto,
            username: username, password: password,
        )
        Task {
            do {
                if let mp = existing {
                    try await state.api.updateManualProxy(id: mp.id, input)
                } else {
                    try await state.api.addManualProxy(input)
                }
                await state.refreshAll()
                dismiss()
            } catch APIError.manualNameInUse {
                errorText = "Name “\(name)” is already used by another manual proxy."
                submitting = false
            } catch {
                errorText = error.localizedDescription
                submitting = false
            }
        }
    }
}
