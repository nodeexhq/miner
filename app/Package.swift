// swift-tools-version:6.0
import PackageDescription

// NVTO Node, the macOS app: a menu-bar program that runs the Go core
// (cmd/nvto-node) and the virtual machine a renter is given.
let package = Package(
    name: "NVTONode",
    platforms: [.macOS(.v15)],
    dependencies: [
        // Sparkle updates the app. It is the one dependency, and the usual
        // choice for a Mac app distributed outside the App Store.
        .package(url: "https://github.com/sparkle-project/Sparkle", from: "2.6.0"),
    ],
    targets: [
        // Everything that is not user interface: supervising the core,
        // virtual machines, port forwarding.
        .target(name: "NodeKit"),
        // The menu-bar app.
        .executableTarget(
            name: "NVTONode",
            dependencies: ["NodeKit", .product(name: "Sparkle", package: "Sparkle")],
            // Sparkle.framework is copied into the app by scripts/bundle.sh.
            linkerSettings: [.unsafeFlags(["-Xlinker", "-rpath", "-Xlinker", "@executable_path/../Frameworks"])]),
        // The same virtual-machine code from a terminal, for preparing the
        // base image and for testing.
        .executableTarget(name: "nvto-vm", dependencies: ["NodeKit"]),
        .testTarget(name: "NodeKitTests", dependencies: ["NodeKit"]),
    ],
    swiftLanguageModes: [.v5]
)
