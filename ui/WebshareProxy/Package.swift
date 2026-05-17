// swift-tools-version:5.9
import PackageDescription

let package = Package(
    name: "WebshareProxy",
    platforms: [.macOS(.v13)],
    targets: [
        .executableTarget(
            name: "WebshareProxy",
            path: "Sources/WebshareProxy"
        )
    ]
)
