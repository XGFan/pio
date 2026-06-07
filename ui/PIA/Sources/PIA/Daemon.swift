// Daemon.swift — Spawn the bundled `piad run` binary as a
// child process, read the api.port file once it appears, and forward
// shutdown signals on quit.

import Foundation

final class DaemonSupervisor {
    private var process: Process?
    let dataDir: URL

    init() {
        let fm = FileManager.default
        let support = fm.urls(for: .applicationSupportDirectory, in: .userDomainMask).first!
        self.dataDir = support.appendingPathComponent("pia", isDirectory: true)
        try? fm.createDirectory(at: self.dataDir, withIntermediateDirectories: true)
    }

    // start fork-execs the daemon and returns once api.port is readable
    // (or throws after timeout).
    func start() throws {
        // Look for daemon binary in the app bundle's Contents/MacOS first,
        // then PATH for dev runs (`swift run` puts it nowhere obvious so
        // dev users `cp piad ./build/`).
        let exe = locateDaemonBinary()
        guard let exe else {
            throw NSError(domain: "PIA", code: 1, userInfo: [
                NSLocalizedDescriptionKey: "Could not locate piad binary. Run scripts/build-app.sh first."
            ])
        }
        // Remove any stale api.port from a previous run so waitForAPIPort
        // doesn't latch onto an old port the new daemon hasn't claimed.
        let portFile = dataDir.appendingPathComponent("api.port")
        try? FileManager.default.removeItem(at: portFile)
        let proc = Process()
        proc.executableURL = exe
        proc.arguments = ["run", "--data-dir=\(dataDir.path)"]
        proc.standardOutput = FileHandle.nullDevice
        proc.standardError = FileHandle.nullDevice
        try proc.run()
        self.process = proc
        Self.log("daemon spawned pid=\(proc.processIdentifier) exe=\(exe.path)")
    }

    // Wait up to 5s for the daemon to write api.port and return its port.
    func waitForAPIPort(timeout: TimeInterval = 10) throws -> Int {
        let portFile = dataDir.appendingPathComponent("api.port")
        let deadline = Date().addingTimeInterval(timeout)
        while Date() < deadline {
            if let raw = try? String(contentsOf: portFile, encoding: .utf8) {
                let trimmed = raw.trimmingCharacters(in: .whitespacesAndNewlines)
                if let port = Int(trimmed), port > 0 {
                    Self.log("waitForAPIPort got port=\(port)")
                    return port
                }
            }
            Thread.sleep(forTimeInterval: 0.1)
        }
        Self.log("waitForAPIPort timeout after \(Int(timeout))s")
        throw NSError(domain: "PIA", code: 2, userInfo: [
            NSLocalizedDescriptionKey: "Daemon did not write api.port within \(Int(timeout))s"
        ])
    }

    static func log(_ s: String) {
        let line = "[\(Date())] \(s)\n"
        if let data = line.data(using: .utf8) {
            let url = URL(fileURLWithPath: "/tmp/pia.log")
            if let fh = try? FileHandle(forWritingTo: url) {
                fh.seekToEndOfFile(); fh.write(data); try? fh.close()
            } else {
                try? data.write(to: url)
            }
        }
    }

    // shutdown attempts a graceful POST /api/v1/shutdown via api, then
    // SIGTERMs the process if still alive after 2s.
    func shutdown(api: APIClient) async {
        await api.shutdown()
        if let proc = process {
            // Give it ~2s to exit cleanly.
            let deadline = Date().addingTimeInterval(2)
            while proc.isRunning && Date() < deadline {
                try? await Task.sleep(nanoseconds: 100_000_000)
            }
            if proc.isRunning {
                proc.terminate()
            }
        }
    }

    // locateDaemonBinary searches the .app bundle and a few dev-friendly
    // fallbacks. Returns nil if no candidate exists.
    private func locateDaemonBinary() -> URL? {
        // 1. Same directory as the running app's executable.
        if let main = Bundle.main.executableURL {
            let sibling = main.deletingLastPathComponent().appendingPathComponent("piad")
            if FileManager.default.isExecutableFile(atPath: sibling.path) {
                return sibling
            }
        }
        // 2. PIA_PROXYD_BINARY env override for development.
        if let env = ProcessInfo.processInfo.environment["PIA_PROXYD_BINARY"],
           FileManager.default.isExecutableFile(atPath: env) {
            return URL(fileURLWithPath: env)
        }
        // 3. ./piad next to the cwd.
        let cwd = URL(fileURLWithPath: FileManager.default.currentDirectoryPath)
            .appendingPathComponent("piad")
        if FileManager.default.isExecutableFile(atPath: cwd.path) {
            return cwd
        }
        return nil
    }
}
