import AppKit
import MinerKit
import SwiftUI

// The menu-bar app: it runs the Go core, keeps a virtual machine ready, and
// shows the owner what the node is doing.

@MainActor
final class MinerModel: ObservableObject {
    @Published var coreRunning = false
    @Published var nodeStatus = "unknown"
    @Published var gate = "off"
    @Published var lease: String?
    @Published var leaseEnds: Date?
    @Published var machine = "not started"
    @Published var lastLog = ""

    private var core: CoreSupervisor?
    private var machines: MachineController?

    func start() {
        guard core == nil else { return }
        guard let binary = Self.coreBinary() else {
            lastLog = "The nodeex-miner program was not found inside the app."
            return
        }
        do {
            let controller = try MachineController()
            controller.onChange = { [weak self] state in self?.machine = Self.describe(state) }
            machines = controller
            Task { await controller.renew() }
        } catch {
            machine = "unavailable: \(error.localizedDescription)"
        }

        // Renters are sent to the virtual machine through the controller's
        // ports, never to this Mac's own SSH or screen sharing.
        // NODEEX_MINER_DIR keeps a development run away from the node this
        // Mac is really enrolled as.
        let stateDirectory = ProcessInfo.processInfo.environment["NODEEX_MINER_DIR"].map { URL(fileURLWithPath: $0, isDirectory: true) }
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
        machine = "stopped"
    }

    private func handle(_ event: CoreEvent) {
        switch event.event {
        case "node": nodeStatus = event.state ?? "unknown"
        case "gate": gate = event.state ?? "off"
        case "lease_started":
            lease = event.lease
            leaseEnds = event.endsAt
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
        case .ready(let address): return "ready at \(address)"
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

struct MenuContent: View {
    @ObservedObject var model: MinerModel

    var body: some View {
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
        Divider()
        Button("Open the mining dashboard") { NSWorkspace.shared.open(URL(string: "https://app.nodeex.com/mining")!) }
        Divider()
        Button("Quit NodeEx Miner") {
            Task {
                await model.stop()
                NSApplication.shared.terminate(nil)
            }
        }
    }
}

struct MinerApp: App {
    @StateObject private var model = MinerModel()

    var body: some Scene {
        MenuBarExtra {
            MenuContent(model: model).onAppear { model.start() }
        } label: {
            Image(systemName: model.lease != nil ? "desktopcomputer.and.arrow.down" : model.gate == "open" ? "desktopcomputer" : "desktopcomputer.trianglebadge.exclamationmark")
                .onAppear { model.start() }
        }
    }
}

MinerApp.main()
