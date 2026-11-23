import AppKit
import NodeKit
import ServiceManagement
import Sparkle
import SwiftUI
import Virtualization

// The menu-bar app: it takes a Mac from nothing to a working node, then
// runs the Go core, keeps a virtual machine ready, and shows the owner what
// the node is doing.

@MainActor
final class NodeModel: ObservableObject {
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
    /// The machine was cut off the network for abuse, and what has come of
    /// telling the platform.
    @Published var abuse: AbuseRecord?
    @Published var abuseStatus = ""
    /// The owner's emergency stop: the lease it is for (fixed when the
    /// window opens), and how it went.
    enum StopState: Equatable {
        case confirming
        case stopping
        case done(StopLeaseReply)
        case failed(String)
    }
    @Published var stopLease: String?
    @Published var stopState: StopState = .confirming
    /// An emergency stop switched hosting off; the owner turns it on again in
    /// the dashboard. Until then the node gets no new leases (and if one
    /// comes, hosting is evidently on again).
    @Published var hostingOff = false
    @Published var pausedUntil: Date?
    private var reporter: AbuseReporter?

    private let store = VMStore.standard
    private var core: CoreSupervisor?
    private var machines: MachineController?
    private var leaseSupervisor: LeaseSupervisor?
    private var coreGeneration = UUID()
    private var sleepObserver: NSObjectProtocol?
    private var setupRunner: VMRunner?
    let updates = Updates()
    private var started = false
    /// Renter connections open, and when that last changed: an update
    /// during a lease waits until nobody has been connected for a while.
    @Published var sessions = 0
    var sessionsChanged = Date()
    /// Whether a renter could be cut off by restarting now.
    var renterActive: Bool {
        lease != nil && (sessions > 0 || Date().timeIntervalSince(sessionsChanged) < 600)
    }
    /// Keeps the Mac from sleeping while the node runs.
    private var awake: NSObjectProtocol?
    private var triedEnvironmentCode = false

    private var environment: [String: String] { ProcessInfo.processInfo.environment }
    /// NVTO_NODE_DIR keeps a development run away from the node this Mac
    /// is really enrolled as.
    private var stateDirectory: URL? { environment["NVTO_NODE_DIR"].map { URL(fileURLWithPath: $0, isDirectory: true) } }
    private var api: String { environment["NVTO_API"] ?? "https://app.nvto.com" }

    /// What the machine's disk will be on this Mac, for the setup window.
    var diskPlan: String {
        let free = store.freeGB()
        guard let disk = VMResources.diskGB(forFreeGB: free) else { return "\(VMError.notEnoughDisk(freeGB: free))".prefix(1).uppercased() + "\(VMError.notEnoughDisk(freeGB: free))".dropFirst() + "." }
        return "This Mac has \(free) GB free. The machine's disk will be \(disk) GB, which leaves room for your own macOS even if a renter fills theirs."
    }

    var needsWindow: Bool {
        switch phase {
        case .checking, .running: return false
        default: return true
        }
    }

    /// Works out what is missing and either asks for it or starts hosting.
    func start() {
        guard !started else { return }
        started = true
        updates.start(model: self)
        advance()
    }

    private func advance() {
        guard let binary = Self.coreBinary() else {
            phase = .failed("The nvto-node program was not found inside the app.")
            return
        }
        let cli = CoreCLI(binary: binary, stateDirectory: stateDirectory)
        guard cli.isEnrolled else {
            // A code in the environment enrolls without a window, for
            // setting up a Mac from a script. It is tried once.
            if let code = environment["NVTO_ENROLL_CODE"], !code.isEmpty, !triedEnvironmentCode {
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
                    if case .success = result { self.advance() } else { self.beginAutomaticSetup() }
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
        let api = api, force = environment["NVTO_ENROLL_FORCE"] != nil
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
                // Decided before 17 GB are downloaded for nothing.
                let resources = try VMInstaller.resources(for: store)
                let image = try await VMInstaller.restoreImage(store: store) { fraction in
                    Task { @MainActor in self.phase = .downloading(fraction) }
                }
                phase = .installing(0)
                try await VMInstaller.install(image: image, into: store.base, resources: resources) { fraction in
                    Task { @MainActor in self.phase = .installing(fraction) }
                }
                // The image is 17 GB and has done its job.
                try? FileManager.default.removeItem(at: store.restoreImage)
                beginAutomaticSetup()
            } catch {
                // A half-made machine is of no use; the next attempt starts clean.
                try? FileManager.default.removeItem(at: store.base.directory)
                phase = .failed("The machine could not be created: \(error)")
            }
        }
    }

    /// How long the machine is given to set itself up before a person is
    /// asked to do it.
    static let automaticSetupLimit: TimeInterval = 20 * 60
    private var triedAutomaticSetup = false

    /// The machine's first-run setup without a person: the host writes a
    /// script onto the stopped machine's disk, the machine runs it at its
    /// first boot and shuts down. Tried once per launch; if the machine has
    /// not shut down in time, or cannot be prepared, the owner is shown the
    /// machine and asked to do the steps by hand, as before.
    func beginAutomaticSetup() {
        guard !triedAutomaticSetup, setupRunner == nil else { phase = .guestSetup; return }
        triedAutomaticSetup = true
        phase = .finishing
        let store = store
        Task {
            let prepared = await Task.detached { (try? Provisioning.prepareFirstRun(store: store)) ?? false }.value
            guard prepared, let runner = try? VMRunner(bundle: store.base, resources: .forThisHost()) else {
                phase = .guestSetup
                return
            }
            runner.onStop = { [weak self] _ in
                guard let self, self.setupRunner === runner else { return }
                self.setupRunner = nil
                self.finishGuestSetup()
            }
            setupRunner = runner
            do { try await runner.start() } catch {
                setupRunner = nil
                phase = .guestSetup
                return
            }
            try? await Task.sleep(nanoseconds: UInt64(Self.automaticSetupLimit * 1_000_000_000))
            guard setupRunner === runner else { return }
            NSLog("NVTO machine: automatic first-run setup did not finish; asking the owner")
            runner.onStop = nil
            await runner.stop()
            setupRunner = nil
            note = "The machine could not set itself up. Start it and do the steps below."
            phase = .guestSetup
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
        // A node that falls asleep is offline to renters. While the node
        // runs, idle sleep is held off (the display may still sleep). It is
        // released when the app quits; macOS ends it if the app crashes.
        if awake == nil {
            awake = ProcessInfo.processInfo.beginActivity(
                options: [.idleSystemSleepDisabled, .suddenTerminationDisabled],
                reason: "NVTO Node is serving renters")
        }
        do {
            let controller = try MachineController(store: store)
            controller.onChange = { [weak self] state in self?.machine = Self.describe(state) }
            // A machine cut off for abuse is reported to the platform, which
            // ends the lease; the machine is discarded on its word, like any
            // lease that ends.
            let cli = CoreCLI(binary: binary, stateDirectory: stateDirectory)
            let reporter = AbuseReporter(store: store) { record in
                await Task.detached {
                    AbuseOutcome(cli.reportAbuse(lease: record.lease, reason: record.reason, detail: record.detail))
                }.value
            }
            reporter.onOutcome = { [weak self] _, outcome in self?.abuseStatus = Self.describe(outcome) }
            controller.onNetworkAbuse = { [weak self] record in
                self?.abuse = record
                self?.abuseStatus = "telling the platform"
                reporter.reportPending()
            }
            if let pending = AbuseRecord.load(store), !pending.reported {
                abuse = pending
                abuseStatus = "telling the platform"
            }
            reporter.reportPending()
            self.reporter = reporter
            machines = controller
            // Old renter disks remain powered off until the platform has
            // confirmed which lease (if any) is currently entitled to run.
            leaseSupervisor = LeaseSupervisor(machine: controller)
        } catch {
            machine = "unavailable: \(error)"
        }
        // Renters are sent to the virtual machine through the controller's
        // ports, never to this Mac's own SSH or screen sharing.
        let supervisor = CoreSupervisor(options: .init(
            binary: binary,
            stateDirectory: stateDirectory,
            sshTarget: "127.0.0.1:\(MachineController.sshPort)",
            vncTarget: "127.0.0.1:\(MachineController.vncPort)",
            machine: machines?.machineSize))
        let generation = UUID()
        coreGeneration = generation
        // Preserve callback delivery order: unstructured MainActor tasks
        // can overtake one another around a core exit/restart.
        supervisor.onRunning = { [weak self] running in DispatchQueue.main.async {
            guard let self, self.coreGeneration == generation else { return }
            self.coreRunning = running
            if !running {
                self.gate = "off"
                self.leaseSupervisor?.suspend()
            }
        } }
        supervisor.onLog = { [weak self] line in DispatchQueue.main.async {
            guard let self, self.coreGeneration == generation else { return }
            self.lastLog = line
        } }
        supervisor.onEvent = { [weak self] event in DispatchQueue.main.async {
            guard let self, self.coreGeneration == generation else { return }
            self.handle(event)
        } }
        sleepObserver = NSWorkspace.shared.notificationCenter.addObserver(
            forName: NSWorkspace.willSleepNotification, object: nil, queue: .main
        ) { [weak self] _ in
            MainActor.assumeIsolated { self?.leaseSupervisor?.suspend() }
        }
        core = supervisor
        supervisor.start()
    }

    /// The disk a rebuilt machine would have on this Mac now, in GB.
    var rebuiltDiskGB: UInt64? {
        // The base counts as free: it is replaced. (The current machine is
        // an APFS copy of it and shares its blocks, so it is not added.)
        let machines = (try? FileManager.default.allocatedSizeOfDirectory(at: store.base.directory)) ?? 0
        return VMResources.diskGB(forFreeGB: store.freeGB() + machines >> 30)
    }

    /// The disk the current machine has, in GB.
    var currentDiskGB: Int? { machines?.machineSize?.diskGB }

    /// Throws away the base machine and builds a new one, for instance to
    /// give renters the larger disk this version of the app allows. Only
    /// between leases. macOS is downloaded again unless an image is at hand,
    /// and the new machine needs its first-run setup again.
    func rebuildMachine() {
        guard lease == nil, phase == .running else { return }
        let alert = NSAlert()
        alert.messageText = "Rebuild the renters' machine?"
        let size = rebuiltDiskGB.map { "\($0) GB" } ?? "the size this Mac allows"
        alert.informativeText = "The machine renters get is deleted and made again, with a disk of \(size)" + (currentDiskGB.map { " instead of \($0) GB" } ?? "") + ". This downloads macOS again (about 17 GB) unless a .ipsw file is in /Users/Shared or Downloads, and you will set up the new machine once more (an account, then Remote Login and Screen Sharing), from Continue setup in this menu. The node takes no orders until it is done."
        alert.addButton(withTitle: "Rebuild")
        alert.addButton(withTitle: "Cancel")
        NSApplication.shared.activate(ignoringOtherApps: true)
        guard alert.runModal() == .alertFirstButtonReturn else { return }
        Task {
            await stop()
            try? FileManager.default.removeItem(at: store.current.directory)
            try? FileManager.default.removeItem(at: store.base.directory)
            try? FileManager.default.removeItem(at: store.root.appendingPathComponent("account"))
            started = true
            phase = .machineIntro
            NSLog("NVTO machine: rebuilding at the owner's request")
        }
    }

    func stop() async {
        coreGeneration = UUID()
        core?.stop()
        core = nil
        coreRunning = false
        if let sleepObserver {
            NSWorkspace.shared.notificationCenter.removeObserver(sleepObserver)
            self.sleepObserver = nil
        }
        await leaseSupervisor?.stop()
        leaseSupervisor = nil
        machines = nil
        await setupRunner?.stop()
        machine = "stopped"
    }

    private func handle(_ event: CoreEvent) {
        switch event.event {
        case "node":
            nodeStatus = event.state ?? "unknown"
        case "gate":
            gate = event.state ?? "off"
            if event.state == "suspended" { leaseSupervisor?.suspend() }
        case "sessions":
            sessions = event.sessions ?? 0
            sessionsChanged = Date()
        case "bandwidth":
            MachineController.setBandwidth(upMbps: event.upMbps ?? 0, downMbps: event.downMbps ?? 0, store: store)
        case "lease_state":
            guard coreRunning, leaseSupervisor?.receive(event) == true else { return }
            let previous = lease
            lease = event.lease
            leaseEnds = event.endsAt
            if previous != nil, lease == nil {
                updates.leaseEnded()
                abuse = nil
            }
            // A new lease means hosting is on again.
            if let lease, lease != previous { hostingOff = false; pausedUntil = nil }
        default: break
        }
    }

    /// Opens the emergency stop for the lease now running.
    func beginStop() {
        guard let lease else { return }
        stopLease = lease
        stopState = .confirming
    }

    /// The owner confirmed the emergency stop.
    func confirmStop(reason: String) {
        guard let lease = stopLease, stopState == .confirming, let binary = Self.coreBinary() else { return }
        stopState = .stopping
        let cli = CoreCLI(binary: binary, stateDirectory: stateDirectory)
        Task.detached {
            let outcome = StopLeaseOutcome(cli.stopLease(lease: lease, reason: reason))
            await MainActor.run { self.stopped(outcome) }
        }
    }

    private func stopped(_ outcome: StopLeaseOutcome) {
        switch outcome {
        case .stopped(let reply):
            NSLog("NVTO: the owner stopped lease %@; the renter was refunded %@", reply.leaseID, reply.refunded)
            stopState = .done(reply)
            hostingOff = !reply.hosting
            pausedUntil = reply.pausedUntil
            // The lease ends, and its machine is erased, on the next
            // heartbeat: the platform reports no lease.
        case .leaseEnded:
            stopState = .failed("The lease has already ended.")
        case .failed(let why):
            stopState = .failed("The lease was not stopped: \(why). Try again, or stop it from the hosting dashboard.")
        }
    }

    static func describe(_ outcome: AbuseOutcome) -> String {
        switch outcome {
        case .reported: return "reported; the platform ended the lease"
        case .leaseEnded: return "the lease had already ended"
        case .refused(let why): return "the platform refused the report: \(why)"
        case .failed(let why): return "not reported yet (\(why)); trying again"
        }
    }

    static func describe(_ state: MachineController.State) -> String {
        switch state {
        case .noBase: return "no base machine prepared"
        case .starting: return "starting a fresh machine"
        case .suspended: return "stopped, waiting for lease confirmation"
        case .paused: return "paused, waiting for lease confirmation"
        case .ready: return "ready, on its own filtered network"
        case .failed(let why): return "failed: \(why)"
        }
    }

    /// The core is shipped inside the app; NVTO_NODE_BIN overrides it
    /// during development.
    static func coreBinary() -> URL? {
        let fm = FileManager.default
        if let override = ProcessInfo.processInfo.environment["NVTO_NODE_BIN"], fm.isExecutableFile(atPath: override) {
            return URL(fileURLWithPath: override)
        }
        let bundled = Bundle.main.bundleURL.appendingPathComponent("Contents/Helpers/nvto-node")
        if fm.isExecutableFile(atPath: bundled.path) { return bundled }
        let beside = Bundle.main.bundleURL.appendingPathComponent("nvto-node")
        return fm.isExecutableFile(atPath: beside.path) ? beside : nil
    }
}

/// Keeps the app up to date. Updates are found, downloaded and installed
/// without asking, because a node is a machine nobody sits at; but an update
/// restarts the app, and the app is what keeps a renter's machine running,
/// so nothing is installed while a lease is on.
@MainActor
final class Updates: NSObject, SPUUpdaterDelegate, ObservableObject {
    private var controller: SPUStandardUpdaterController?
    private weak var model: NodeModel?
    private var resume: (() -> Void)?
    /// The version of an update that is ready but waiting for the lease to
    /// end, shown in the menu with a way to install it now.
    @Published private(set) var waiting: String?

    /// False for a build that carries no feed address (a development run).
    var available: Bool { controller != nil }

    func start(model: NodeModel) {
        self.model = model
        // A waiting update goes ahead once no renter has been connected for
        // ten minutes: a restart then costs a renter nothing, and their
        // machine and its disk come back as they were.
        Timer.scheduledTimer(withTimeInterval: 60, repeats: true) { [weak self] _ in
            MainActor.assumeIsolated {
                guard let self, self.waiting != nil, !self.shouldWait() else { return }
                self.installNow()
            }
        }
        guard controller == nil, Bundle.main.object(forInfoDictionaryKey: "SUFeedURL") != nil else { return }
        controller = SPUStandardUpdaterController(startingUpdater: true, updaterDelegate: self, userDriverDelegate: nil)
        // Sparkle's first check comes some time after launch; a test of
        // the update path asks for one at once.
        if ProcessInfo.processInfo.environment["NVTO_CHECK_UPDATES_NOW"] != nil {
            controller?.updater.checkForUpdatesInBackground()
        }
    }

    func check() {
        // The owner asked: what they then choose to install goes in now.
        ownerAskedAt = Date()
        controller?.checkForUpdates(nil)
    }

    /// When the owner last checked for updates from the menu. An update they
    /// install from that check (within half an hour) is not held back for a
    /// renter: they chose the moment. Later, automatic updates wait again.
    private var ownerAskedAt: Date?
    private var ownerAsked: Bool { ownerAskedAt.map { Date().timeIntervalSince($0) < 1800 } ?? false }
    /// When an update first had to wait for a renter.
    private var waitingSince: Date?

    /// Whether an update should wait now. Not when the owner asked for it;
    /// otherwise while a renter is active, but no longer than a day: after
    /// that it goes in during the small hours (03:00 to 05:00, this Mac's
    /// time), because a renter's machine and disk survive the restart and
    /// a connection that looks open may be one that died without saying so.
    private func shouldWait() -> Bool {
        if ownerAsked { return false }
        guard model?.renterActive == true else { return false }
        let since = waitingSince ?? Date()
        waitingSince = since
        if Date().timeIntervalSince(since) > 24 * 3600 {
            let hour = Calendar.current.component(.hour, from: Date())
            if (3..<5).contains(hour) { return false }
        }
        return true
    }

    /// The lease ended: an update that was waiting may go ahead.
    func leaseEnded() {
        installNow()
    }

    /// Installs a waiting update at once, relaunching the app. During a lease
    /// the renter's connection drops for the minute this takes.
    func installNow() {
        waiting = nil
        waitingSince = nil
        resume?()
        resume = nil
    }

    /// An update is downloaded and would wait for the app to quit. A node
    /// never quits on its own, so it is installed, with a relaunch, as
    /// soon as no renter would be cut off: now, or when the lease ends.
    nonisolated func updater(_ updater: SPUUpdater, willInstallUpdateOnQuit item: SUAppcastItem, immediateInstallationBlock immediateInstallHandler: @escaping () -> Void) -> Bool {
        MainActor.assumeIsolated {
            if shouldWait() {
                resume = immediateInstallHandler
                waiting = item.displayVersionString
            } else {
                DispatchQueue.main.async { immediateInstallHandler() }
            }
            return true
        }
    }

    nonisolated func updater(_ updater: SPUUpdater, shouldPostponeRelaunchForUpdate item: SUAppcastItem, untilInvokingBlock installHandler: @escaping () -> Void) -> Bool {
        MainActor.assumeIsolated {
            guard shouldWait() else { return false }
            resume = installHandler
            waiting = item.displayVersionString
            return true
        }
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
    @ObservedObject var model: NodeModel
    @State private var code = ""

    var body: some View {
        VStack(alignment: .leading, spacing: 16) {
            if model.setupMachine == nil {
                HStack(spacing: 10) {
                    Image(nsImage: NSApplication.shared.applicationIconImage).resizable().frame(width: 36, height: 36)
                    Text("NVTO Node").font(.headline)
                }
            }
            switch model.phase {
            case .checking, .finishing:
                ProgressView(model.phase == .finishing ? "Preparing the machine" : "Checking this Mac")
            case .enroll, .enrolling:
                Text("Connect this Mac").font(.title2).bold()
                Text("Open the hosting dashboard at app.nvto.com, choose Add a node, and paste the code it gives you. The code works once.")
                TextField("Enroll code", text: $code).textFieldStyle(.roundedBorder).disabled(model.phase == .enrolling)
                HStack {
                    Button("Open the dashboard") { NSWorkspace.shared.open(URL(string: "https://app.nvto.com/hosting")!) }
                    Spacer()
                    Button(model.phase == .enrolling ? "Connecting" : "Connect") { model.enroll(code: code) }
                        .keyboardShortcut(.defaultAction).disabled(model.phase == .enrolling || code.isEmpty)
                }
            case .machineIntro:
                Text("Create the machine renters will use").font(.title2).bold()
                Text("A renter never uses your macOS. They get a virtual machine that is created for their lease and erased when it ends. NVTO Node downloads macOS from Apple for it, about 17 GB.")
                Text(model.diskPlan).foregroundStyle(.secondary)
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
                Text("This Mac is an NVTO node").font(.title2).bold()
                Text("NVTO Node keeps running in the menu bar. You can close this window.")
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
    @ObservedObject var model: NodeModel
    @ObservedObject var updates: Updates
    @Environment(\.openWindow) private var openWindow
    @State private var startsAtLogin = SMAppService.mainApp.status == .enabled

    /// The version, so an owner can see an update arrive.
    private var version: String {
        Bundle.main.object(forInfoDictionaryKey: "CFBundleShortVersionString") as? String ?? "development build"
    }

    var body: some View {
        Text("NVTO Node \(version)")
        Divider()
        if model.phase == .running {
            Text("Node: \(model.nodeStatus)")
            Text(model.coreRunning ? "Core: running" : "Core: stopped")
            Text("Remote access: \(model.gate)")
            Text("Machine: \(model.machine)")
            if let lease = model.lease {
                Text("Rented: lease \(lease.prefix(8))" + (model.leaseEnds.map { ", until \($0.formatted(date: .abbreviated, time: .shortened))" } ?? ""))
            } else {
                Text("Not rented")
            }
            if let abuse = model.abuse {
                Text("The renter's machine was cut off the network for abuse (\(abuse.reason.replacingOccurrences(of: "_", with: " "))): \(model.abuseStatus)")
                    .lineLimit(3)
            }
            if model.hostingOff {
                Text("Hosting is off. Turn it on again in the hosting dashboard"
                     + (model.pausedUntil.map { "; new orders resume \($0.formatted(date: .abbreviated, time: .shortened))" } ?? "") + ".")
                    .lineLimit(3)
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
        if model.phase == .running && model.lease != nil {
            Button("Stop this lease now…") {
                model.beginStop()
                openWindow(id: "stop-lease")
                NSApplication.shared.activate(ignoringOtherApps: true)
            }
        }
        if model.phase == .running && model.lease == nil {
            Button("Rebuild the renters' machine…") { model.rebuildMachine() }
        }
        Button("Open the hosting dashboard") { NSWorkspace.shared.open(URL(string: "https://app.nvto.com/hosting")!) }
        if let version = updates.waiting {
            // Ready, but held back so that a renter is not cut off.
            Text("Update \(version) will install once no renter has been connected for 10 minutes")
            Button("Install update now (the renter reconnects in a minute)") { updates.installNow() }
        }
        if model.updates.available {
            Button("Check for Updates…") { model.updates.check() }
        }
        Toggle("Start at login", isOn: Binding(
            get: { startsAtLogin },
            set: { on in
                // A node is of use only while it is on: starting with the
                // Mac is the normal state, and the owner's to switch off.
                do {
                    if on { try SMAppService.mainApp.register() } else { try SMAppService.mainApp.unregister() }
                } catch {
                    NSLog("NVTO: could not change the login item: \(error)")
                }
                startsAtLogin = SMAppService.mainApp.status == .enabled
            }))
        Divider()
        Button("Quit NVTO Node") {
            Task {
                await model.stop()
                NSApplication.shared.terminate(nil)
            }
        }
    }
}

/// The owner's emergency stop: what it costs, said plainly, before it is done.
struct StopLeaseView: View {
    @ObservedObject var model: NodeModel
    @State private var reason = ""
    @Environment(\.dismiss) private var dismiss

    var body: some View {
        VStack(alignment: .leading, spacing: 14) {
            switch model.stopState {
            case .confirming, .stopping:
                Text("Stop this lease now?").font(.title2).bold()
                if let lease = model.stopLease {
                    Text("Lease \(lease.prefix(8)) ends at once, and the renter's machine is erased.")
                }
                VStack(alignment: .leading, spacing: 6) {
                    Text("• The renter gets this period's rent back in full.")
                    Text("• You are paid nothing for this period.")
                    Text("• Hosting on this node is switched off until you turn it back on in the hosting dashboard.")
                    Text("• Two stops within 30 days pause this node for 7 days.")
                }
                .fixedSize(horizontal: false, vertical: true)
                TextField("Reason (optional)", text: $reason, axis: .vertical)
                    .lineLimit(3...5)
                    .textFieldStyle(.roundedBorder)
                    .disabled(model.stopState == .stopping)
                HStack {
                    Spacer()
                    Button("Cancel") { dismiss() }.keyboardShortcut(.cancelAction).disabled(model.stopState == .stopping)
                    Button(model.stopState == .stopping ? "Stopping…" : "Stop the lease") { model.confirmStop(reason: reason) }
                        .disabled(model.stopState == .stopping || model.stopLease == nil)
                }
            case .done(let reply):
                Text("The lease is stopped").font(.title2).bold()
                Text("The renter was refunded $\(reply.refunded), this period's rent in full. You are paid nothing for it.")
                Text("Hosting on this node is now off. Turn it on again in the hosting dashboard when you are ready.")
                if let until = reply.pausedUntil {
                    Text("This node was stopped twice within 30 days: it takes no new orders until \(until.formatted(date: .abbreviated, time: .shortened)).")
                }
                Text("The renter's machine is erased as soon as the platform confirms the lease has ended, within a minute.")
                    .foregroundStyle(.secondary)
                HStack {
                    Button("Open the hosting dashboard") { NSWorkspace.shared.open(URL(string: "https://app.nvto.com/hosting")!) }
                    Spacer()
                    Button("Done") { dismiss() }.keyboardShortcut(.defaultAction)
                }
            case .failed(let why):
                Text("The lease was not stopped").font(.title2).bold()
                Text(why).textSelection(.enabled)
                HStack {
                    Spacer()
                    Button("Close") { dismiss() }.keyboardShortcut(.defaultAction)
                }
            }
        }
        .fixedSize(horizontal: false, vertical: true)
        .padding(24)
        .frame(width: 480, alignment: .leading)
    }
}

/// Opens the setup window at launch when there is setup to do.
struct Launcher: View {
    @ObservedObject var model: NodeModel
    @Environment(\.openWindow) private var openWindow

    var body: some View {
        // The NVTO mark: with a dot while rented, dimmed while the node
        // is not open to renters.
        Image(nsImage: MenuBarMark.image(style: .tile, badge: model.lease != nil ? .dot : model.gate == "open" ? .none : .dimmed))
            .onAppear {
                model.start()
                if model.needsWindow {
                    openWindow(id: "setup")
                    NSApplication.shared.activate(ignoringOtherApps: true)
                }
            }
    }
}

struct NodeApp: App {
    @StateObject private var model = NodeModel()

    var body: some Scene {
        MenuBarExtra {
            MenuContent(model: model, updates: model.updates)
        } label: {
            Launcher(model: model)
        }
        Window("NVTO Node", id: "setup") {
            SetupView(model: model)
        }
        .windowResizability(.contentSize)
        Window("Stop this lease", id: "stop-lease") {
            StopLeaseView(model: model)
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

NodeApp.main()
