// swift-tools-version:6.0
import PackageDescription

// The macOS app around the Miner: a menu-bar program that runs the Go core
// (cmd/nodeex-miner) and the virtual machine a renter is given.
let package = Package(
    name: "NodeExMiner",
    platforms: [.macOS(.v15)],
    dependencies: [
        // Sparkle updates the app. It is the one dependency, and the usual
        // choice for a Mac app distributed outside the App Store.
        .package(url: "https://github.com/sparkle-project/Sparkle", from: "2.6.0"),
    ],
    targets: [
        // Everything that is not user interface: supervising the core,
        // virtual machines, port forwarding.
        .target(name: "MinerKit"),
        // The menu-bar app.
        .executableTarget(
            name: "NodeExMiner",
            dependencies: ["MinerKit", .product(name: "Sparkle", package: "Sparkle")],
            // Sparkle.framework is copied into the app by scripts/bundle.sh.
            linkerSettings: [.unsafeFlags(["-Xlinker", "-rpath", "-Xlinker", "@executable_path/../Frameworks"])]),
        // The same virtual-machine code from a terminal, for preparing the
        // base image and for testing.
        .executableTarget(name: "nodeex-vm", dependencies: ["MinerKit"]),
        .testTarget(name: "MinerKitTests", dependencies: ["MinerKit"]),
    ],
    swiftLanguageModes: [.v5]
)
