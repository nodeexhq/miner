import Foundation

/// The controller is also exercised with a blocked start/stop in tests,
/// without constructing a real Virtualization.framework machine.
@MainActor
protocol VMRuntime: AnyObject {
    var onStop: ((Error?) -> Void)? { get set }
    func disconnect()
    func start() async throws
    func stop() async
    func sshEndpoint(timeout: TimeInterval) async throws -> VMRunner.Endpoint
}

extension VMRunner: VMRuntime {
    func disconnect() { network?.stop() }
}

/// Keeps one fresh virtual machine ready for a renter, keeps it (disk and
/// all) for that renter through restarts while the lease runs, and replaces
/// it for another lease: a renter's used machine is never handed on.
@MainActor
public final class MachineController: LeaseMachine {
    public enum State: Equatable, Sendable {
        /// No base machine has been prepared on this Mac.
        case noBase
        case starting
        case suspended
        /// The machine answers; the host reaches it at this loopback port.
        case ready(sshPort: UInt16)
        case failed(String)
    }

    /// Loopback ports the Go core forwards renters to; the controller points
    /// them at whichever machine is current.
    public static let sshPort: UInt16 = 22022
    public static let vncPort: UInt16 = 25900

    public private(set) var state: State = .noBase {
        didSet { if state != oldValue { onChange?(state) } }
    }
    public var onChange: ((State) -> Void)?

    private let store: VMStore
    private let resources: VMResources
    typealias RunnerFactory = (VMBundle, VMResources, @escaping @Sendable () -> Void) throws -> any VMRuntime
    private let makeRunner: RunnerFactory
    private var runner: (any VMRuntime)?
    private var generation = 0
    private var wantedKeys: [String] = []
    private var permitted = false
    private var authorizedLeaseID: String?
    private var keyTask: Task<Void, Never>?
    private var installedKeys: [String]?
    private var stopTask: Task<Void, Never>?

    public convenience init(store: VMStore = .standard, resources: VMResources = .forThisHost()) throws {
        // A renter's machine is never started on the unfiltered network.
        guard let helper = GuestNetwork.helper() else { throw VMError.unsupportedHost("the nodeex-vmnet helper is missing") }
        self.init(store: store, resources: resources) { bundle, resources, onNetworkExit in
            guard let mac = try? String(contentsOf: bundle.macAddress, encoding: .utf8).trimmingCharacters(in: .whitespacesAndNewlines) else {
                throw VMError.damagedBundle("MAC address")
            }
            let network = try GuestNetwork(helper: helper, macAddress: mac, sshPort: Self.sshPort, vncPort: Self.vncPort)
            network.onExit = onNetworkExit
            return try VMRunner(bundle: bundle, resources: resources, network: network)
        }
    }

    init(store: VMStore, resources: VMResources, makeRunner: @escaping RunnerFactory) {
        self.store = store
        self.resources = resources
        self.makeRunner = makeRunner
    }

    /// The size of the machine leases are given: this host's share of
    /// cores and memory, and the disk the base machine was installed with.
    /// Nil until there is a base machine.
    public var machineSize: MachineSize? {
        guard store.base.exists, let bytes = try? FileManager.default.attributesOfItem(atPath: store.base.disk.path)[.size] as? UInt64 else { return nil }
        return MachineSize(cpuCount: resources.cpuCount, memoryGB: Int(resources.memoryBytes >> 30), diskGB: Int(bytes >> 30))
    }

    // A renter keeps their machine for the whole lease: when the app or
    // the Mac restarts, or the renter shuts it down, the same machine (the
    // same disk) starts again. Without authorization it stays off; the
    // next lease gets a fresh copy of the base.

    /// Called only by LeaseSupervisor after a fresh platform confirmation.
    /// Retain a disk only for the SAME lease. Idle guests are never booted:
    /// lease:null means no current authorization, not necessarily final expiry.
    public func run(leaseID: String) async {
        guard !leaseID.isEmpty else { await suspend(); return }
        permitted = true
        authorizedLeaseID = leaseID
        let reset = !store.current.exists || store.currentLease != leaseID
        await boot(reset: reset, for: leaseID)
    }

    /// Starts a machine: a fresh copy of the base when reset is true (marked
    /// as belonging to lease, if given), otherwise the current one as it is.
    private func boot(reset: Bool, for lease: String) async {
        generation += 1
        let mine = generation
        installedKeys = nil
        state = .starting
        await stopCurrent()
        guard generation == mine, permitted else { return }
        guard store.base.exists else {
            state = .noBase
            return
        }
        state = .starting
        do {
            if reset || !store.current.exists {
                try store.resetCurrent()
                try store.assignCurrentLease(lease)
            }
            // The helper forwards the two loopback ports for as long as this
            // machine lives, and applies the outbound rules to it.
            let next = try makeRunner(store.current, resources) { [weak self] in
                Task { @MainActor in
                    guard let self, self.generation == mine, self.permitted else { return }
                    self.state = .failed("the machine's network stopped")
                    self.retry(mine)
                }
            }
            next.onStop = { [weak self] error in
                guard let self, self.generation == mine, self.permitted else { return }
                // The renter shut the machine down, or it crashed. During a
                // lease it starts again with its disk as it was.
                self.state = .failed(error.map { "\($0.localizedDescription)" } ?? "the machine was shut down")
                Task { if self.generation == mine, self.permitted { await self.restart() } }
            }
            runner = next
            try await next.start()
            guard generation == mine, permitted else { await next.stop(); return }
            let endpoint = try await next.sshEndpoint(timeout: 180)
            guard generation == mine, permitted else { await next.stop(); return }
            state = .ready(sshPort: endpoint.port)
            failures = 0
            NSLog("NodeEx machine: ready%@, SSH on 127.0.0.1:%d", store.currentLease.map { " for lease \($0)" } ?? "", endpoint.port)
            applyKeys()
        } catch {
            guard generation == mine, permitted else { return }
            state = .failed("\(error)")
            NSLog("NodeEx machine: could not be made ready: %@", "\(error)")
            retry(mine)
        }
    }

    /// Restarts only the currently authorized lease's machine.
    private func restart() async {
        guard permitted, let id = authorizedLeaseID else { return }
        // A failed ownership write must not turn a retry into an unowned
        // guest. The platform authorization, not disk metadata, selects it.
        await boot(reset: !store.current.exists || store.currentLease != id, for: id)
    }

    /// Consecutive failures to make a machine ready, for spacing out retries.
    private var failures = 0

    /// Tries again after a failure: soon at first, then less often (30 s,
    /// 1, 2, 4 … up to 10 minutes), so a passing problem fixes itself and a
    /// lasting one does not spin.
    private func retry(_ generation: Int) {
        failures += 1
        let delay = min(30.0 * pow(2.0, Double(failures - 1)), 600)
        NSLog("NodeEx machine: retrying in %.0f s (failure %d)", delay, failures)
        Task { @MainActor [weak self] in
            try? await Task.sleep(nanoseconds: UInt64(delay * 1_000_000_000))
            guard let self, self.generation == generation, self.permitted else { return }
            await self.restart()
        }
    }

    /// Installs the renter's SSH keys in the machine that is running, in
    /// place of any that were there. With no keys only the host can log in.
    /// The keys are remembered and applied to a machine still starting.
    public func authorize(renterKeys: [String]) {
        wantedKeys = renterKeys
        applyKeys()
    }

    private func applyKeys() {
        guard permitted, keyTask == nil, installedKeys != wantedKeys, case .ready(let port) = state,
              let account = Provisioning.account(store: store) else { return }
        let endpoint = VMRunner.Endpoint(host: "127.0.0.1", port: port)
        let keys = wantedKeys, store = store
        let mine = generation
        keyTask = Task {
            let succeeded = await Task.detached {
                do {
                    try Provisioning.authorize(renterKeys: keys, at: endpoint, account: account, store: store)
                    return true
                } catch {
                    NSLog("NodeEx: could not install the renter's keys: \(error)")
                    return false
                }
            }.value
            guard generation == mine else { return }
            keyTask = nil
            if succeeded { installedKeys = keys }
            if wantedKeys != keys { applyKeys() }
        }
    }

    /// Revocation cuts the network before any asynchronous power-off. A
    /// guest still starting cannot keep Jump/UU or another outbound tunnel.
    public func interrupt() {
        generation += 1
        permitted = false
        authorizedLeaseID = nil
        wantedKeys = []
        installedKeys = nil
        runner?.onStop = nil
        runner?.disconnect()
    }

    public func suspend() async {
        interrupt()
        state = .suspended
        await stopCurrent()
    }

    /// Every path that detaches a runner, including a retry/reboot, publishes
    /// the pending stop before yielding. Later revocation and starts must
    /// wait for it even though `runner` is already nil.
    private func stopCurrent() async {
        let mine = generation
        let old = runner
        old?.onStop = nil
        old?.disconnect()
        runner = nil
        let pendingKeys = keyTask
        let previousStop = stopTask
        // A second revocation must wait for the first power-off too, even
        // after it detached the old runner. Never replace an open disk.
        let stop = Task {
            await previousStop?.value
            await old?.stop()
            await pendingKeys?.value
        }
        stopTask = stop
        await stop.value
        if generation == mine { keyTask = nil; stopTask = nil }
    }

    public func shutDown() async { await suspend() }
}
