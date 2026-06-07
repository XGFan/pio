// swift-tools-version:5.9
import PackageDescription

let package = Package(
    name: "PIA",
    platforms: [.macOS(.v13)],
    targets: [
        .executableTarget(
            name: "PIA",
            path: "Sources/PIA"
        )
    ]
)
