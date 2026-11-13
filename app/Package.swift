// swift-tools-version:6.0
import PackageDescription

// The macOS app around the Miner: a menu-bar program that runs the Go core
// (cmd/nodeex-miner) and the virtual machine a renter is given.
let package = Package(
    name: "NodeExMiner",
    platforms: [.macOS(.v15)],
    targets: [
        // Everything that is not user interface: supervising the core,
        // virtual machines, port forwarding.
        .target(name: "MinerKit"),
        // The menu-bar app.
        .executableTarget(name: "NodeExMiner", dependencies: ["MinerKit"]),
        // The same virtual-machine code from a terminal, for preparing the
        // base image and for testing.
        .executableTarget(name: "nodeex-vm", dependencies: ["MinerKit"]),
        .testTarget(name: "MinerKitTests", dependencies: ["MinerKit"]),
    ],
    swiftLanguageModes: [.v5]
)
