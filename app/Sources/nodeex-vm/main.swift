import AppKit
import MinerKit
import Virtualization

// nodeex-vm prepares and tests the virtual machine from a terminal.
//
//   nodeex-vm status           what is on disk
//   nodeex-vm install          download macOS and install the base machine
//   nodeex-vm setup            boot the base in a window, for first-run setup
//   nodeex-vm provision        let the host log in to the base and use sudo there
//   nodeex-vm run              boot a fresh copy of the base and print its address
//
// The program must be signed with the com.apple.security.virtualization
// entitlement (scripts/bundle.sh does it).

// Progress lines must appear as they happen, also when output is a file.
setvbuf(stdout, nil, _IOLBF, 0)

let store = VMStore.standard
let resources = VMResources.forThisHost()

/// Machines this process started. A machine stops when nothing refers to it.
@MainActor var running: [VMRunner] = []

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
            print("for a rented machine: \(resources.cpuCount) cores, \(resources.memoryBytes >> 30) GB memory")
            if store.base.exists, let size = try? FileManager.default.attributesOfItem(atPath: store.base.disk.path)[.size] as? UInt64 {
                print("its disk: \(size >> 30) GB, fixed when the base machine was installed")
            } else if let disk = VMResources.diskGB(forFreeGB: store.freeGB()) {
                print("its disk would be: \(disk) GB (\(store.freeGB()) GB free on this Mac)")
            } else {
                print("\(VMError.notEnoughDisk(freeGB: store.freeGB()))")
            }
            exit(0)
        case "install":
            if store.base.exists { fail("a base machine already exists at \(store.base.directory.path)") }
            let sized = try VMInstaller.resources(for: store)
            let image = try await VMInstaller.restoreImage(store: store, progress: percent("downloading macOS"))
            print("installing macOS \(image.operatingSystemVersion.majorVersion).\(image.operatingSystemVersion.minorVersion) (build \(image.buildVersion))")
            try await VMInstaller.install(image: image, into: store.base, resources: sized, progress: percent("installing"))
            print("installed. Next: nodeex-vm setup")
            exit(0)
        case "provision":
            let account = try Provisioning.prepareBase(store: store)
            print("the base machine is prepared: the host can log in as \(account) and administrators have password-less sudo")
            exit(0)
        case "setup":
            guard store.base.exists else { fail("\(VMError.noBaseImage)") }
            let runner = try VMRunner(bundle: store.base, resources: resources)
            runner.onStop = { _ in exit(0) }
            running.append(runner)
            try await runner.start()
            showWindow(for: runner, title: "NodeEx base machine: finish setup, then shut it down")
            print("Set up the account, turn on Remote Login and Screen Sharing, then shut the machine down from its Apple menu.")
        case "run":
            try store.resetCurrent()
            // On the filtered network when the helper is there, as a
            // renter's machine always is; --nat asks for the system's NAT.
            var network: GuestNetwork?
            if !CommandLine.arguments.contains("--nat"), let helper = GuestNetwork.helper() {
                let mac = try String(contentsOf: store.current.macAddress, encoding: .utf8).trimmingCharacters(in: .whitespacesAndNewlines)
                network = try GuestNetwork(helper: helper, macAddress: mac, sshPort: MachineController.sshPort, vncPort: MachineController.vncPort)
            }
            let runner = try VMRunner(bundle: store.current, resources: resources, network: network)
            runner.onStop = { error in
                print("machine stopped\(error.map { ": \($0.localizedDescription)" } ?? "")")
                exit(0)
            }
            running.append(runner)
            try await runner.start()
            print("started a fresh copy of the base; waiting for its address")
            let endpoint = try await runner.sshEndpoint()
            print("ssh: \(endpoint.host) port \(endpoint.port)\(network == nil ? " (system NAT)" : " (filtered network)")")
            if let at = CommandLine.arguments.firstIndex(of: "--authorize"), CommandLine.arguments.count > at + 1, let account = Provisioning.account(store: store) {
                // nodeex-vm run --authorize FILE: let the public keys in FILE log in, as a renter's would.
                let keys = try String(contentsOfFile: CommandLine.arguments[at + 1], encoding: .utf8).split(separator: "\n").map(String.init)
                try Provisioning.authorize(renterKeys: keys, at: endpoint, account: account, store: store)
                print("authorized \(keys.count) key(s) for \(account)")
            }
            if let at = CommandLine.arguments.firstIndex(of: "--exec"), CommandLine.arguments.count > at + 2 {
                // nodeex-vm run --exec ACCOUNT COMMAND: run a command in the guest and stop.
                let output = try Provisioning.guest(CommandLine.arguments[at + 2], at: endpoint, account: CommandLine.arguments[at + 1], store: store)
                print(output, terminator: "")
                await runner.stop()
                exit(0)
            }
            if CommandLine.arguments.contains("--window") { showWindow(for: runner, title: "NodeEx machine") }
        default:
            fail("usage: nodeex-vm <status|install|setup|provision|run [--nat] [--window] [--exec ACCOUNT COMMAND]>")
        }
    } catch {
        fail("\(error)")
    }
}

Task { @MainActor in await main() }
NSApplication.shared.run()
