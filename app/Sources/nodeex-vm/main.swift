import AppKit
import MinerKit
import Virtualization

// nodeex-vm prepares and tests the virtual machine from a terminal.
//
//   nodeex-vm status           what is on disk
//   nodeex-vm install          download macOS and install the base machine
//   nodeex-vm setup            boot the base in a window, for first-run setup
//   nodeex-vm run              boot a fresh copy of the base and print its address
//
// The program must be signed with the com.apple.security.virtualization
// entitlement (scripts/bundle.sh does it).

let store = VMStore.standard
let resources = VMResources.forThisHost()

func fail(_ message: String) -> Never {
    FileHandle.standardError.write(Data((message + "\n").utf8))
    exit(1)
}

func percent(_ label: String) -> @Sendable (Double) -> Void {
    let last = Counter()
    return { fraction in
        let whole = Int(fraction * 100)
        if last.advance(to: whole) { print("\(label): \(whole)%") }
    }
}

final class Counter: @unchecked Sendable {
    private var value = -1
    private let lock = NSLock()
    func advance(to next: Int) -> Bool {
        lock.lock()
        defer { lock.unlock() }
        guard next > value else { return false }
        value = next
        return true
    }
}

/// Shows a machine in a window until the window is closed or the guest stops.
@MainActor
func showWindow(for runner: VMRunner, title: String) {
    let app = NSApplication.shared
    app.setActivationPolicy(.regular)
    let view = VZVirtualMachineView()
    view.virtualMachine = runner.machine
    view.capturesSystemKeys = true
    let window = NSWindow(contentRect: NSRect(x: 0, y: 0, width: 1280, height: 800), styleMask: [.titled, .closable, .resizable, .miniaturizable], backing: .buffered, defer: false)
    window.title = title
    window.contentView = view
    window.center()
    window.makeKeyAndOrderFront(nil)
    app.activate(ignoringOtherApps: true)
    NotificationCenter.default.addObserver(forName: NSWindow.willCloseNotification, object: window, queue: .main) { _ in
        Task { @MainActor in
            await runner.stop()
            exit(0)
        }
    }
}

@MainActor
func main() async {
    let command = CommandLine.arguments.dropFirst().first ?? "status"
    do {
        switch command {
        case "status":
            print("machines: \(store.root.path)")
            print("restore image: \(FileManager.default.fileExists(atPath: store.restoreImage.path) ? "downloaded" : "not downloaded")")
            print("base machine: \(store.base.exists ? "installed" : "not installed")")
            print("for a rented machine: \(resources.cpuCount) cores, \(resources.memoryBytes >> 30) GB memory, \(resources.diskBytes >> 30) GB disk (sparse)")
            exit(0)
        case "install":
            if store.base.exists { fail("a base machine already exists at \(store.base.directory.path)") }
            let image = try await VMInstaller.restoreImage(store: store, progress: percent("downloading macOS"))
            print("installing macOS \(image.operatingSystemVersion.majorVersion).\(image.operatingSystemVersion.minorVersion) (build \(image.buildVersion))")
            try await VMInstaller.install(image: image, into: store.base, resources: resources, progress: percent("installing"))
            print("installed. Next: nodeex-vm setup")
            exit(0)
        case "setup":
            guard store.base.exists else { fail("\(VMError.noBaseImage)") }
            let runner = try VMRunner(bundle: store.base, resources: resources)
            runner.onStop = { _ in exit(0) }
            try await runner.start()
            showWindow(for: runner, title: "NodeEx base machine: finish setup, then shut it down")
            print("Set up the account, turn on Remote Login and Screen Sharing, then shut the machine down from its Apple menu.")
        case "run":
            try store.resetCurrent()
            let runner = try VMRunner(bundle: store.current, resources: resources)
            runner.onStop = { error in
                print("machine stopped\(error.map { ": \($0.localizedDescription)" } ?? "")")
                exit(0)
            }
            try await runner.start()
            print("started a fresh copy of the base; waiting for its address")
            print("address: \(try await runner.address())")
            if CommandLine.arguments.contains("--window") { showWindow(for: runner, title: "NodeEx machine") }
        default:
            fail("usage: nodeex-vm <status|install|setup|run [--window]>")
        }
    } catch {
        fail("\(error)")
    }
}

Task { @MainActor in await main() }
NSApplication.shared.run()
