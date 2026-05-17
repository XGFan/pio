// App.swift — Menubar-only SwiftUI app. The main window is openable from
// the menubar but is not on the Dock (LSUIElement=true via Info.plist).

import AppKit
import SwiftUI

@main
struct WebshareProxyApp: App {
    @NSApplicationDelegateAdaptor(AppDelegate.self) var appDelegate

    var body: some Scene {
        MenuBarExtra("Webshare Proxy", systemImage: "network") {
            MenuContent().environmentObject(AppState.shared)
        }
        .menuBarExtraStyle(.menu)

        Window("Webshare Proxy", id: "main") {
            MainWindow().environmentObject(AppState.shared)
        }
        .windowResizability(.contentSize)
        .handlesExternalEvents(matching: ["show"])
    }
}

// AppDelegate owns the single AppState instance and handles
// `webshareproxy://show` URLs reliably regardless of whether the SwiftUI
// scene has materialized yet. Owning AppState here avoids the SwiftUI
// @StateObject-on-App pitfall where the object can be reinitialized as
// scenes recompute, leaving views bound to a non-bootstrapped state.
final class AppDelegate: NSObject, NSApplicationDelegate {
    func applicationDidFinishLaunching(_ notification: Notification) {
        Task { @MainActor in
            await AppState.shared.bootstrap()
        }
    }

    func application(_ application: NSApplication, open urls: [URL]) {
        for url in urls where url.scheme == "webshareproxy" && url.host == "show" {
            relocateMainWindowToCursorScreen()
        }
    }

    private func relocateMainWindowToCursorScreen() {
        let cursor = NSEvent.mouseLocation
        Task { @MainActor in
            for _ in 0..<40 {
                if let w = NSApp.windows.first(where: { $0.title == "Webshare Proxy" }) {
                    let target = NSScreen.screens.first(where: { $0.frame.contains(cursor) }) ?? NSScreen.main
                    if let f = target?.visibleFrame {
                        let origin = NSPoint(
                            x: f.midX - w.frame.width / 2,
                            y: f.midY - w.frame.height / 2
                        )
                        w.setFrameOrigin(origin)
                    }
                    w.makeKeyAndOrderFront(nil)
                    NSApp.activate(ignoringOtherApps: true)
                    return
                }
                try? await Task.sleep(nanoseconds: 50_000_000)
            }
        }
    }
}

struct MenuContent: View {
    @EnvironmentObject var state: AppState
    @Environment(\.openWindow) private var openWindow

    var body: some View {
        Text(state.statusMessage).font(.caption)
        Text(state.proxyRunning ? "Proxy: Running" : "Proxy: Stopped").font(.caption)
        Divider()
        Button(state.proxyRunning ? "Stop Proxy" : "Start Proxy") {
            Task {
                if state.proxyRunning {
                    await state.stopProxy()
                } else {
                    await state.startProxy()
                }
            }
        }
        Button("Open Window…") {
            openWindow(id: "main")
            NSApp.activate(ignoringOtherApps: true)
        }
        Button("Sync All Now") {
            Task {
                for key in state.keys {
                    try? await state.api.syncKey(id: key.id)
                }
                await state.refreshAll()
            }
        }
        if !state.users.isEmpty {
            Divider()
            // Top 3 users surface in the menubar; hover a username to drop
            // into its submenu of upstream choices. Drag-to-reorder in the
            // Users & Rules tab controls which three appear here.
            ForEach(state.users.prefix(3)) { user in
                Menu(user.username) {
                    Button(menuLabel("— Unmapped", isSelected: user.upstreamProxyId == nil)) {
                        setMapping(user.username, to: nil)
                    }
                    ForEach(state.upstreams) { up in
                        Button(menuLabel(up.displayName, isSelected: user.upstreamProxyId == up.id)) {
                            setMapping(user.username, to: up.id)
                        }
                    }
                }
            }
        }
        Divider()
        Button("Quit") {
            Task {
                await state.shutdown()
                NSApplication.shared.terminate(nil)
            }
        }.keyboardShortcut("q")
    }

    // Returns the label with a leading checkmark when selected, or
    // equivalent leading whitespace when not — so rows in the same submenu
    // align vertically regardless of selection.
    private func menuLabel(_ text: String, isSelected: Bool) -> String {
        isSelected ? "✓ \(text)" : "   \(text)"
    }

    private func setMapping(_ username: String, to upstreamID: String?) {
        Task {
            try? await state.api.setUserMapping(username: username, upstreamId: upstreamID)
            await state.refreshAll()
        }
    }
}
