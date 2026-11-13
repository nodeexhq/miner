import AppKit
import MinerKit
import ServiceManagement
import SwiftUI
import Virtualization

// The menu-bar app: it takes a Mac from nothing to a working node, then
// runs the Go core, keeps a virtual machine ready, and shows the owner what
// the node is doing.

@MainActor
final class MinerModel: ObservableObject {
    /// Where the owner is on the way to a running node.
    enum Phase: Equatable {
        case checking
        /// Not enrolled: a code from the dashboard is needed.
        case enroll
        case enrolling
        /// Enrolled, but there is no machine to rent out yet.
        case machineIntro
        case downloading(Double)
        case installing(Double)
        /// macOS is installed in the machine and waits for its first-run setup.
        case guestSetup
        case finishing
        case running
        case failed(String)
    }

    @Published var phase: Phase = .checking
    @Published var note = ""
    @Published var coreRunning = false
    @Published var nodeStatus = "unknown"
    @Published var gate = "off"
    @Published var lease: String?
    @Published var leaseEnds: Date?
    @Published var machine = "not started"
    @Published var lastLog = ""
    /// The machine shown in the setup window during its first-run setup.
    @Published var setupMachine: VZVirtualMachine?

    private let store = VMStore.standard
    private var core: CoreSupervisor?
    private var machines: MachineController?
    private var setupRunner: VMRunner?
    private var started = false
    private var triedEnvironmentCode = false

    private var environment: [String: String] { ProcessInfo.processInfo.environment }
    /// NODEEX_MINER_DIR keeps a development run away from the node this Mac
    /// is really enrolled as.
    private var stateDirectory: URL? { environment["NODEEX_MINER_DIR"].map { URL(fileURLWithPath: $0, isDirectory: true) } }
    private var api: String { environment["NODEEX_API"] ?? "https://app.nodeex.com" }

    var needsWindow: Bool {
        switch phase {
        case .checking, .running: return false
        default: return true
        }
    }

    /// Works out what is missing and either asks for it or starts mining.
    func start() {
        guard !started else { return }
        started = true
        advance()
    }

    private func advance() {
        guard let binary = Self.coreBinary() else {
            phase = .failed("The nodeex-miner program was not found inside the app.")
            return
        }
        let cli = CoreCLI(binary: binary, stateDirectory: stateDirectory)
        guard cli.isEnrolled else {
            // A code in the environment enrolls without a window, for
            // setting up a Mac from a script. It is tried once.
            if let code = environment["NODEEX_ENROLL_CODE"], !code.isEmpty, !triedEnvironmentCode {
                triedEnvironmentCode = true
                enroll(code: code)
            } else {
                phase = .enroll
            }
            return
        }
        guard store.base.exists else {
            phase = .machineIntro
            return
        }
        if Provisioning.account(store: store) == nil {
            // Installed, perhaps set up; preparing it tells which.
            phase = .finishing
            let store = store
            Task.detached {
                let result = Result { try Provisioning.prepareBase(store: store) }
                await MainActor.run {
                    if case .success = result { self.advance() } else { self.phase = .guestSetup }
                }
            }
            return
        }
        run(binary: binary)
    }

    func enroll(code: String) {
        guard let binary = Self.coreBinary(), !code.trimmingCharacters(in: .whitespaces).isEmpty else { return }
        phase = .enrolling
        note = ""
        let cli = CoreCLI(binary: binary, stateDirectory: stateDirectory)
        let api = api, force = environment["NODEEX_ENROLL_FORCE"] != nil
        Task.detached {
            let result = cli.enroll(code: code, api: api, force: force)
            await MainActor.run {
                if result.succeeded {
                    self.advance()
                } else {
                    self.note = result.message
                    self.phase = .enroll
                }
            }
        }
    }

    /// Downloads macOS from Apple and installs it in a new machine.
    func createMachine() {
        phase = .downloading(0)
        note = ""
        Task {
            do {
                let image = try await VMInstaller.restoreImage(store: store) { fraction in
                    Task { @MainActor in self.phase = .downloading(fraction) }
                }
                phase = .installing(0)
                try await VMInstaller.install(image: image, into: store.base, resources: .forThisHost()) { fraction in
                    Task { @MainActor in self.phase = .installing(fraction) }
                }
                // The image is 17 GB and has done its job.
                try? FileManager.default.removeItem(at: store.restoreImage)
                phase = .guestSetup
            } catch {
                // A half-made machine is of no use; the next attempt starts clean.
                try? FileManager.default.removeItem(at: store.base.directory)
                phase = .failed("The machine could not be created: \(error)")
            }
        }
    }

    /// Boots the new machine in the setup window for its first-run setup.
    func beginGuestSetup() {
        guard setupRunner == nil else { return }
        do {
            let runner = try VMRunner(bundle: store.base, resources: .forThisHost())
            runner.onStop = { [weak self] _ in
                guard let self else { return }
                self.setupRunner = nil
                self.setupMachine = nil
                self.finishGuestSetup()
            }
            setupRunner = runner
            setupMachine = runner.machine
            Task {
                do { try await runner.start() } catch {
                    self.setupRunner = nil
                    self.setupMachine = nil
                    self.phase = .failed("The machine did not start: \(error.localizedDescription)")
                }
            }
        } catch {
            phase = .failed("\(error)")
        }
    }

    private func finishGuestSetup() {
        phase = .finishing
        let store = store
        Task.detached {
            let result = Result { try Provisioning.prepareBase(store: store) }
            await MainActor.run {
                switch result {
                case .success: self.advance()
                case .failure(let error):
                    self.note = "The machine is not ready yet: \(error). Start it again and finish the steps."
                    self.phase = .guestSetup
                }
            }
        }
    }

    func retry() {
        note = ""
        advance()
    }

    private func run(binary: URL) {
        guard core == nil else { return }
        phase = .running
        do {
            let controller = try MachineController(store: store)
            controller.onChange = { [weak self] state in self?.machine = Self.describe(state) }
            machines = controller
            Task { await controller.renew() }
        } catch {
            machine = "unavailable: \(error)"
        }
        // Renters are sent to the virtual machine through the controller's
        // ports, never to this Mac's own SSH or screen sharing.
        let supervisor = CoreSupervisor(options: .init(
            binary: binary,
            stateDirectory: stateDirectory,
            sshTarget: "127.0.0.1:\(MachineController.sshPort)",
            vncTarget: "127.0.0.1:\(MachineController.vncPort)"))
        supervisor.onRunning = { [weak self] running in Task { @MainActor in self?.coreRunning = running; if !running { self?.gate = "off" } } }
        supervisor.onLog = { [weak self] line in Task { @MainActor in self?.lastLog = line } }
        supervisor.onEvent = { [weak self] event in Task { @MainActor in self?.handle(event) } }
        core = supervisor
        supervisor.start()
    }

    func stop() async {
        core?.stop()
        core = nil
        await machines?.shutDown()
        machines = nil
        await setupRunner?.stop()
        machine = "stopped"
    }

    private func handle(_ event: CoreEvent) {
        switch event.event {
        case "node": nodeStatus = event.state ?? "unknown"
        case "gate": gate = event.state ?? "off"
        case "lease_started":
            lease = event.lease
            leaseEnds = event.endsAt
        case "lease_keys":
            // The renter's way in to the machine this lease was given.
            machines?.authorize(renterKeys: event.sshKeys ?? [])
        case "lease_ended":
            lease = nil
            leaseEnds = nil
            // The machine that renter used is destroyed and replaced.
            if let machines { Task { await machines.renew() } }
        default: break
        }
    }

    static func describe(_ state: MachineController.State) -> String {
        switch state {
        case .noBase: return "no base machine prepared"
        case .starting: return "starting a fresh machine"
        case .ready: return "ready, on its own filtered network"
        case .failed(let why): return "failed: \(why)"
        }
    }

    /// The core is shipped inside the app; NODEEX_MINER_BIN overrides it
    /// during development.
    static func coreBinary() -> URL? {
        let fm = FileManager.default
        if let override = ProcessInfo.processInfo.environment["NODEEX_MINER_BIN"], fm.isExecutableFile(atPath: override) {
            return URL(fileURLWithPath: override)
        }
        let bundled = Bundle.main.bundleURL.appendingPathComponent("Contents/Helpers/nodeex-miner")
        if fm.isExecutableFile(atPath: bundled.path) { return bundled }
        let beside = Bundle.main.bundleURL.appendingPathComponent("nodeex-miner")
        return fm.isExecutableFile(atPath: beside.path) ? beside : nil
    }
}

/// The framework's view of a running machine, for the setup window.
struct MachineView: NSViewRepresentable {
    let machine: VZVirtualMachine

    func makeNSView(context: Context) -> VZVirtualMachineView {
        let view = VZVirtualMachineView()
        view.capturesSystemKeys = true
        view.virtualMachine = machine
        return view
    }

    func updateNSView(_ view: VZVirtualMachineView, context: Context) {
        view.virtualMachine = machine
    }
}

struct SetupView: View {
    @ObservedObject var model: MinerModel
    @State private var code = ""

    var body: some View {
        VStack(alignment: .leading, spacing: 16) {
            switch model.phase {
            case .checking, .finishing:
                ProgressView(model.phase == .finishing ? "Preparing the machine" : "Checking this Mac")
            case .enroll, .enrolling:
                Text("Connect this Mac").font(.title2).bold()
                Text("Open the mining dashboard at app.nodeex.com, choose Add a node, and paste the code it gives you. The code works once.")
                TextField("Enroll code", text: $code).textFieldStyle(.roundedBorder).disabled(model.phase == .enrolling)
                HStack {
                    Button("Open the dashboard") { NSWorkspace.shared.open(URL(string: "https://app.nodeex.com/mining")!) }
                    Spacer()
                    Button(model.phase == .enrolling ? "Connecting" : "Connect") { model.enroll(code: code) }
                        .keyboardShortcut(.defaultAction).disabled(model.phase == .enrolling || code.isEmpty)
                }
            case .machineIntro:
                Text("Create the machine renters will use").font(.title2).bold()
                Text("A renter never uses your macOS. They get a virtual machine that is created for their lease and erased when it ends. NodeEx Miner downloads macOS from Apple for it, about 17 GB, and needs about 45 GB of free disk space.")
                HStack {
                    Spacer()
                    Button("Download and create") { model.createMachine() }.keyboardShortcut(.defaultAction)
                }
            case .downloading(let fraction):
                ProgressView("Downloading macOS from Apple", value: fraction)
                Text("\(Int(fraction * 100))%. This can take a while; you can keep using your Mac.").foregroundStyle(.secondary)
            case .installing(let fraction):
                ProgressView("Installing macOS in the machine", value: fraction)
                Text("\(Int(fraction * 100))%").foregroundStyle(.secondary)
            case .guestSetup:
                if let machine = model.setupMachine {
                    Text("In the machine below: create an account (any name and password; skip Apple Account and everything optional), then open System Settings › General › Sharing and turn on Remote Login and Screen Sharing. Then choose Shut Down from its Apple menu.")
                        .fixedSize(horizontal: false, vertical: true)
                    MachineView(machine: machine).frame(minWidth: 960, minHeight: 600)
                } else {
                    Text("Set up the machine, once").font(.title2).bold()
                    Text("macOS is installed. It needs its first-run setup, which only a person can do: create an account, then turn on Remote Login and Screen Sharing in System Settings › General › Sharing, and shut the machine down. It takes about three minutes and is never needed again.")
                    HStack {
                        Spacer()
                        Button("Start the machine") { model.beginGuestSetup() }.keyboardShortcut(.defaultAction)
                    }
                }
            case .failed(let why):
                Text("Something went wrong").font(.title2).bold()
                Text(why).textSelection(.enabled)
                HStack {
                    Spacer()
                    Button("Try again") { model.retry() }.keyboardShortcut(.defaultAction)
                }
            case .running:
                Text("This Mac is a NodeEx node").font(.title2).bold()
                Text("NodeEx Miner keeps running in the menu bar. You can close this window.")
            }
            if !model.note.isEmpty {
                Text(model.note).foregroundStyle(.red).textSelection(.enabled).fixedSize(horizontal: false, vertical: true)
            }
        }
        .padding(24)
        .frame(minWidth: 520, alignment: .leading)
    }
}

struct MenuContent: View {
    @ObservedObject var model: MinerModel
    @Environment(\.openWindow) private var openWindow
    @State private var startsAtLogin = SMAppService.mainApp.status == .enabled

    var body: some View {
        if model.phase == .running {
            Text("Node: \(model.nodeStatus)")
            Text(model.coreRunning ? "Miner: running" : "Miner: stopped")
            Text("Remote access: \(model.gate)")
            Text("Machine: \(model.machine)")
            if let lease = model.lease {
                Text("Rented: lease \(lease.prefix(8))" + (model.leaseEnds.map { ", until \($0.formatted(date: .abbreviated, time: .shortened))" } ?? ""))
            } else {
                Text("Not rented")
            }
            if !model.lastLog.isEmpty {
                Divider()
                Text(model.lastLog).lineLimit(2)
            }
        } else {
            Text("Setup is not finished")
            Button("Continue setup") {
                openWindow(id: "setup")
                NSApplication.shared.activate(ignoringOtherApps: true)
            }
        }
        Divider()
        Button("Open the mining dashboard") { NSWorkspace.shared.open(URL(string: "https://app.nodeex.com/mining")!) }
        Toggle("Start at login", isOn: Binding(
            get: { startsAtLogin },
            set: { on in
                // A node is of use only while it is on: starting with the
                // Mac is the normal state, and the owner's to switch off.
                do {
                    if on { try SMAppService.mainApp.register() } else { try SMAppService.mainApp.unregister() }
                } catch {
                    NSLog("NodeEx: could not change the login item: \(error)")
                }
                startsAtLogin = SMAppService.mainApp.status == .enabled
            }))
        Divider()
        Button("Quit NodeEx Miner") {
            Task {
                await model.stop()
                NSApplication.shared.terminate(nil)
            }
        }
    }
}

/// Opens the setup window at launch when there is setup to do.
struct Launcher: View {
    @ObservedObject var model: MinerModel
    @Environment(\.openWindow) private var openWindow

    var body: some View {
        Image(systemName: model.lease != nil ? "desktopcomputer.and.arrow.down" : model.gate == "open" ? "desktopcomputer" : "desktopcomputer.trianglebadge.exclamationmark")
            .onAppear {
                model.start()
                if model.needsWindow {
                    openWindow(id: "setup")
                    NSApplication.shared.activate(ignoringOtherApps: true)
                }
            }
    }
}

struct MinerApp: App {
    @StateObject private var model = MinerModel()

    var body: some Scene {
        MenuBarExtra {
            MenuContent(model: model)
        } label: {
            Launcher(model: model)
        }
        Window("NodeEx Miner", id: "setup") {
            SetupView(model: model)
        }
        .windowResizability(.contentSize)
    }
}

// Stopping the app by signal (a logout, a script) must stop what it
// started, just as quitting from the menu does.
signal(SIGTERM, SIG_IGN)
let terminationSource = DispatchSource.makeSignalSource(signal: SIGTERM, queue: .main)
terminationSource.setEventHandler { NSApplication.shared.terminate(nil) }
terminationSource.resume()

MinerApp.main()
