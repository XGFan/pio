// swift-tools-version:5.9
import PackageDescription

let package = Package(
    name: "PIO",
    platforms: [.macOS(.v13)],
    targets: [
        .executableTarget(
            name: "PIO",
            path: "Sources/PIO"
        )
    ]
)
