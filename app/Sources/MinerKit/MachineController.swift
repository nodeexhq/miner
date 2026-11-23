import Foundation

/// The controller is also exercised with a blocked start/stop in tests,
/// without constructing a real Virtualization.framework machine.
@MainActor
protocol VMRuntime: AnyObject {
    var onStop: ((Error?) -> Void)? { get set }
    func disconnect()
    /// Drop all traffic without ending the network, for a pause.
    func holdNetwork()
    func releaseNetwork()
    func start() async throws
    func pause() async -> Bool
    func resume() async -> Bool
    func stop() async
    func sshEndpoint(timeout: TimeInterval) async throws -> VMRunner.Endpoint
    /// Calls handler if the machine's network is cut for abuse.
    func observeNetworkAbuse(_ handler: @escaping @Sendable (GuestNetwork.Abuse) -> Void)
}

extension VMRuntime {
    func observeNetworkAbuse(_ handler: @escaping @Sendable (GuestNetwork.Abuse) -> Void) {}
}

extension VMRunner: VMRuntime {
    func disconnect() { network?.stop() }
    func holdNetwork() { network?.hold() }
    func releaseNetwork() { network?.release() }
    func observeNetworkAbuse(_ handler: @escaping @Sendable (GuestNetwork.Abuse) -> Void) { network?.onAbuse = handler }
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
        /// Off, its disk kept, until the lease is confirmed.
        case suspended
        /// Frozen with its memory and without network, until the lease is
        /// confirmed again; powered off if that takes too long.
        case paused
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

    /// Set when the current machine's network helper cut its network for
    /// abuse (scanning, flooding); nil again for the next machine. The cut
    /// stays, whatever the lease does, until the machine is replaced.
    public private(set) var networkAbuse: GuestNetwork.Abuse?
    /// Called with the abuse, recorded (AbuseRecord) for the machine's
    /// lease, for the platform to be told. That lease is never given a
    /// machine again: a new machine would come with a new network helper,
    /// without the cut. The platform ends the lease, and the machine is
    /// discarded when it says so.
    public var onNetworkAbuse: ((AbuseRecord) -> Void)?

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

    // A lease that is not confirmed for a while (the platform is out of
    // reach, the core restarts, the Mac sleeps, a renewal comes late) need
    // not cost the renter what is running: the machine is paused, memory
    // and all, with its network held, and resumes where it was when the
    // same lease is confirmed. Only after pauseLimit is it powered off.
    var pauseLimit: TimeInterval = 30 * 60
    /// The lease whose machine is paused, or being paused.
    private var pausedLease: String?
    private var pauseTask: Task<Bool, Never>?
    /// Advanced by anything that ends a pause, so its power-off timer lapses.
    private var pauseToken = 0
    /// The runner has started and answered, so it can be paused.
    private var running = false
    /// Which runner the callbacks belong to, and for which generation they
    /// were (re)attached: a resumed runner keeps its network callback.
    private var runnerBoot = 0
    private var handlerGeneration = -1
    private var sshPortInUse = MachineController.sshPort

    public convenience init(store: VMStore = .standard, resources: VMResources = .forThisHost()) throws {
        // A renter's machine is never started on the unfiltered network.
        guard let helper = GuestNetwork.helper() else { throw VMError.unsupportedHost("the nodeex-vmnet helper is missing") }
        self.init(store: store, resources: resources) { bundle, resources, onNetworkExit in
            guard let mac = try? String(contentsOf: bundle.macAddress, encoding: .utf8).trimmingCharacters(in: .whitespacesAndNewlines) else {
                throw VMError.damagedBundle("MAC address")
            }
            let network = try GuestNetwork(helper: helper, macAddress: mac, sshPort: Self.sshPort, vncPort: Self.vncPort, limits: Self.bandwidthFile(store))
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
        if barred(leaseID) {
            await refuseBarred(leaseID)
            return
        }
        permitted = true
        authorizedLeaseID = leaseID
        if pausedLease == leaseID, store.currentLease == leaseID, let paused = runner {
            if await resume(paused) { return }
            guard permitted, authorizedLeaseID == leaseID else { return }
        }
        let reset = !store.current.exists || store.currentLease != leaseID
        await boot(reset: reset, for: leaseID)
    }

    /// Whether lease's machine was cut off for abuse.
    private func barred(_ lease: String) -> Bool { AbuseRecord.load(store)?.lease == lease }

    private func refuseBarred(_ lease: String) async {
        NSLog("NVTO machine: lease %@ was cut off for abuse; no machine for it", lease)
        await powerOff()
        state = .failed("cut off the network for abuse; the lease is being ended")
    }

    /// Brings this lease's paused machine back as it was. False if it could
    /// not be resumed; it is then started again from its disk.
    private func resume(_ paused: any VMRuntime) async -> Bool {
        await pauseTask?.value
        generation += 1
        let mine = generation
        pausedLease = nil
        pauseToken += 1
        state = .starting
        let resumed = await paused.resume()
        guard generation == mine, permitted, runner === paused else { return false }
        guard resumed else { return false }
        attach(paused, generation: mine)
        paused.releaseNetwork()
        running = true
        state = .ready(sshPort: sshPortInUse)
        failures = 0
        NSLog("NVTO machine: resumed for lease %@", authorizedLeaseID ?? "?")
        applyKeys()
        return true
    }

    /// The runner's power-off callback, for the generation now permitted.
    private func attach(_ runner: any VMRuntime, generation mine: Int) {
        handlerGeneration = mine
        runner.onStop = { [weak self] error in
            guard let self, self.generation == mine, self.permitted else { return }
            // The renter shut the machine down, or it crashed. During a
            // lease it starts again with its disk as it was.
            self.running = false
            self.state = .failed(error.map { "\($0.localizedDescription)" } ?? "the machine was shut down")
            Task { if self.generation == mine, self.permitted { await self.restart() } }
        }
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
        // A restart (the renter shut it down, the helper stopped) of a
        // machine cut off for abuse would come back with its network.
        if barred(lease) {
            await refuseBarred(lease)
            return
        }
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
            runnerBoot += 1
            let boot = runnerBoot
            let next = try makeRunner(store.current, resources) { [weak self] in
                Task { @MainActor in
                    guard let self, self.runnerBoot == boot, self.handlerGeneration == self.generation, self.permitted else { return }
                    self.running = false
                    self.state = .failed("the machine's network stopped")
                    self.retry(self.generation)
                }
            }
            networkAbuse = nil
            next.observeNetworkAbuse { [weak self] abuse in
                Task { @MainActor in
                    guard let self, self.runnerBoot == boot else { return }
                    let lease = self.store.currentLease
                    NSLog("NVTO machine: network cut for abuse%@: %@ (%@)", lease.map { " in lease \($0)" } ?? "", abuse.reason, abuse.detail)
                    self.networkAbuse = abuse
                    guard let lease, AbuseRecord.load(self.store)?.lease != lease else { return }
                    let record = AbuseRecord(lease: lease, reason: abuse.reason, detail: abuse.detail, time: abuse.time)
                    do { try record.save(self.store) } catch {
                        NSLog("NVTO machine: could not record the abuse: %@", "\(error)")
                    }
                    self.onNetworkAbuse?(record)
                }
            }
            attach(next, generation: mine)
            runner = next
            try await next.start()
            guard generation == mine, permitted else { await next.stop(); return }
            let endpoint = try await next.sshEndpoint(timeout: 180)
            guard generation == mine, permitted else { await next.stop(); return }
            running = true
            sshPortInUse = endpoint.port
            state = .ready(sshPort: endpoint.port)
            failures = 0
            NSLog("NVTO machine: ready%@, SSH on 127.0.0.1:%d", store.currentLease.map { " for lease \($0)" } ?? "", endpoint.port)
            applyKeys()
        } catch {
            guard generation == mine, permitted else { return }
            state = .failed("\(error)")
            NSLog("NVTO machine: could not be made ready: %@", "\(error)")
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
        NSLog("NVTO machine: retrying in %.0f s (failure %d)", delay, failures)
        Task { @MainActor [weak self] in
            try? await Task.sleep(nanoseconds: UInt64(delay * 1_000_000_000))
            guard let self, self.generation == generation, self.permitted else { return }
            await self.restart()
        }
    }

    /// Installs the renter's SSH keys in the machine that is running, in
    /// place of any that were there. With no keys only the host can log in.
    /// The keys are remembered and applied to a machine still starting.
    /// Where the machine's bandwidth limits are kept for its network
    /// helper, which reads them every few seconds.
    /// A file rather than the controller's state, so that limits that come
    /// before any machine is made still apply to it.
    public static func bandwidthFile(_ store: VMStore) -> URL { store.root.appendingPathComponent("bandwidth.json") }

    /// Limits the machine's internet traffic, in megabits per second each
    /// way; 0 is no limit. The platform sets them from the measured line.
    public static func setBandwidth(upMbps: Double, downMbps: Double, store: VMStore) {
        let file = bandwidthFile(store)
        guard upMbps > 0 || downMbps > 0 else {
            try? FileManager.default.removeItem(at: file)
            return
        }
        let body = ["up_mbps": max(0, upMbps), "down_mbps": max(0, downMbps)]
        if let data = try? JSONSerialization.data(withJSONObject: body) {
            try? data.write(to: file, options: .atomic)
        }
    }

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
                    NSLog("NVTO: could not install the renter's keys: \(error)")
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
        // Held, not ended, so that a pause can resume with its network.
        runner?.holdNetwork()
    }

    /// The lease is not confirmed right now: pause the machine if it is up,
    /// power it off otherwise. Its disk is kept either way.
    public func suspend() async {
        interrupt()
        if pausedLease != nil {
            _ = await pauseTask?.value
            if pausedLease != nil { state = .paused }
            return
        }
        guard running, let current = runner, let lease = store.currentLease else {
            state = .suspended
            await stopCurrent()
            return
        }
        pausedLease = lease
        pauseToken += 1
        let token = pauseToken
        let pause = Task { await current.pause() }
        pauseTask = pause
        let paused = await pause.value
        if pauseToken == token { pauseTask = nil }
        guard pauseToken == token, pausedLease == lease, runner === current else { return }
        guard paused else {
            pausedLease = nil
            state = .suspended
            await stopCurrent()
            return
        }
        running = false
        state = .paused
        NSLog("NVTO machine: paused, waiting for lease %@ to be confirmed", lease)
        let limit = pauseLimit
        Task { @MainActor [weak self] in
            try? await Task.sleep(nanoseconds: UInt64(limit * 1_000_000_000))
            guard let self, self.pauseToken == token, self.pausedLease == lease else { return }
            NSLog("NVTO machine: lease %@ not confirmed for %.0f minutes; powering off", lease, limit / 60)
            await self.powerOff()
        }
    }

    /// Powers the machine off; its disk is kept.
    public func powerOff() async {
        interrupt()
        state = .suspended
        await stopCurrent()
    }

    /// No lease is in force: the last renter's machine is destroyed, disk
    /// and all, so nothing of theirs stays on the owner's Mac. Called only
    /// on the platform's confirmation that there is no lease.
    public func discard() async {
        interrupt()
        let mine = generation
        state = .suspended
        await stopCurrent()
        guard generation == mine, !permitted, store.current.exists else { return }
        // The lease has ended: its abuse record has done its work.
        AbuseRecord.remove(store)
        do {
            try store.discardCurrent()
            NSLog("NVTO machine: the last renter's machine was destroyed")
        } catch {
            NSLog("NVTO machine: could not destroy the last renter's machine: %@", "\(error)")
        }
    }

    /// Every path that detaches a runner, including a retry/reboot, publishes
    /// the pending stop before yielding. Later revocation and starts must
    /// wait for it even though `runner` is already nil.
    private func stopCurrent() async {
        // A pause in progress finishes first; a paused machine is stopped
        // like a running one.
        _ = await pauseTask?.value
        pausedLease = nil
        pauseToken += 1
        running = false
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

    public func shutDown() async { await powerOff() }
}
