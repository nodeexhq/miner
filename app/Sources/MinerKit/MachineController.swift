import Foundation

/// Keeps one fresh virtual machine ready for a renter, keeps it (disk and
/// all) for that renter through restarts while the lease runs, and replaces
/// it after the lease: the machine a renter used is destroyed, never handed on.
@MainActor
public final class MachineController {
    public enum State: Equatable, Sendable {
        /// No base machine has been prepared on this Mac.
        case noBase
        case starting
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
    private let helper: URL
    private var runner: VMRunner?
    private var generation = 0
    private var wantedKeys: [String] = []

    public init(store: VMStore = .standard, resources: VMResources = .forThisHost()) throws {
        self.store = store
        self.resources = resources
        // A renter's machine is never started on the unfiltered network.
        guard let helper = GuestNetwork.helper() else { throw VMError.unsupportedHost("the nodeex-vmnet helper is missing") }
        self.helper = helper
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
    // same disk) starts again. It is destroyed only when the lease ends,
    // and the next renter gets a fresh copy of the base.

    /// At launch: the machine of a lease that may still be running is
    /// started as it is; otherwise a fresh one is made. Whether the lease is
    /// in fact still running is known after the core's first heartbeat
    /// (leaseStarted or noLease).
    public func startAtLaunch() async {
        if store.currentLease != nil, store.current.exists {
            await boot(reset: false)
        } else {
            await boot(reset: true)
        }
    }

    /// A lease is running. A machine nobody has had yet becomes this
    /// renter's; one that belonged to another lease is replaced first.
    public func leaseStarted(_ id: String) async {
        switch store.currentLease {
        case id?:
            return
        case nil where store.current.exists:
            store.currentLease = id
        default:
            await boot(reset: true, for: id)
        }
    }

    /// No lease is running (after a restart, the lease may have ended while
    /// the app was not running): a machine a renter had is replaced.
    public func noLease() async {
        if store.currentLease != nil { await renew() }
    }

    /// The lease ended: the machine that renter used is destroyed and a
    /// fresh one made for the next.
    public func renew() async {
        await boot(reset: true)
    }

    /// Starts a machine: a fresh copy of the base when reset is true (marked
    /// as belonging to lease, if given), otherwise the current one as it is.
    private func boot(reset: Bool, for lease: String? = nil) async {
        generation += 1
        let mine = generation
        if reset {
            // The next machine is for the next renter: the last one's keys go.
            wantedKeys = []
        }
        if let old = runner {
            runner = nil
            old.onStop = nil
            await old.stop()
        }
        guard store.base.exists else {
            state = .noBase
            return
        }
        state = .starting
        do {
            if reset || !store.current.exists {
                try store.resetCurrent()
                store.currentLease = lease
            }
            guard let mac = try? String(contentsOf: store.current.macAddress, encoding: .utf8).trimmingCharacters(in: .whitespacesAndNewlines) else {
                throw VMError.damagedBundle("MAC address")
            }
            // The helper forwards the two loopback ports for as long as this
            // machine lives, and applies the outbound rules to it.
            let network = try GuestNetwork(helper: helper, macAddress: mac, sshPort: Self.sshPort, vncPort: Self.vncPort)
            network.onExit = { [weak self] in
                Task { @MainActor in
                    guard let self, self.generation == mine else { return }
                    self.state = .failed("the machine's network stopped")
                    self.retry(mine)
                }
            }
            let next = try VMRunner(bundle: store.current, resources: resources, network: network)
            next.onStop = { [weak self] error in
                guard let self, self.generation == mine else { return }
                // The renter shut the machine down, or it crashed. During a
                // lease it starts again with its disk as it was; between
                // leases a fresh one takes its place.
                self.state = .failed(error.map { "\($0.localizedDescription)" } ?? "the machine was shut down")
                Task { await self.restart() }
            }
            runner = next
            try await next.start()
            let endpoint = try await next.sshEndpoint()
            guard generation == mine else { return }
            state = .ready(sshPort: endpoint.port)
            failures = 0
            NSLog("NodeEx machine: ready%@, SSH on 127.0.0.1:%d", store.currentLease.map { " for lease \($0)" } ?? "", endpoint.port)
            applyKeys()
        } catch {
            guard generation == mine else { return }
            state = .failed("\(error)")
            NSLog("NodeEx machine: could not be made ready: %@", "\(error)")
            retry(mine)
        }
    }

    /// Starts the machine again: the same one during a lease, a fresh one
    /// otherwise.
    private func restart() async {
        await boot(reset: store.currentLease == nil)
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
            guard let self, self.generation == generation else { return }
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
        guard case .ready(let port) = state, let account = Provisioning.account(store: store) else { return }
        let endpoint = VMRunner.Endpoint(host: "127.0.0.1", port: port)
        let keys = wantedKeys, store = store
        Task.detached {
            do {
                try Provisioning.authorize(renterKeys: keys, at: endpoint, account: account, store: store)
            } catch {
                NSLog("NodeEx: could not install the renter's keys: \(error)")
            }
        }
    }

    public func shutDown() async {
        generation += 1
        await runner?.stop()
        runner = nil
    }
}
