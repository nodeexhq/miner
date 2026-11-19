import Foundation

/// The host, not the rented macOS, controls the machine's power and network.
@MainActor
protocol LeaseMachine: AnyObject {
    /// Synchronously cut network access and invalidate pending starts/retries.
    func interrupt()
    /// Take the guest off the network and pause it (or stop it), retaining
    /// its disk and, while paused, its memory, for a renewal.
    func suspend() async
    /// Power the guest off, retaining its disk.
    func powerOff() async
    /// Destroy the guest and its disk: the platform says no lease is held.
    func discard() async
    /// Resume this lease's disk, or prepare a clean guest for another lease.
    func run(leaseID: String) async
    func authorize(renterKeys: [String])
}

/// Enforces platform authorization even when desktop traffic bypasses the
/// NodeEx gate. Nothing boots until a fresh platform snapshot arrives. A
/// core crash, stale heartbeat, or the exact lease deadline cuts the guest's
/// network immediately and stops it; only a fresh confirmation may resume it.
@MainActor
public final class LeaseSupervisor {
    public nonisolated static let confirmationLifetime: TimeInterval = 180

    private enum Target: Equatable { case idle, lease(String) }
    private let machine: LeaseMachine
    private let now: () -> Date
    private let uptime: () -> TimeInterval
    private let lifetime: TimeInterval
    private var target: Target?
    private var deadline: Date?
    private var monotonicDeadline: TimeInterval?
    private var lastConfirmation: Date?
    private var resumeAfter: Date?
    private var renterKeys: [String] = []
    private var revision: UInt64 = 0
    private var timer: Timer?
    private var stopped = false

    public convenience init(machine: MachineController) {
        self.init(machine: machine, now: Date.init)
    }

    init(machine: LeaseMachine, now: @escaping () -> Date,
         uptime: @escaping () -> TimeInterval = { ProcessInfo.processInfo.systemUptime },
         lifetime: TimeInterval = confirmationLifetime, automaticallyCheck: Bool = true) {
        self.machine = machine
        self.now = now
        self.uptime = uptime
        self.lifetime = lifetime
        if automaticallyCheck {
            let timer = Timer(timeInterval: 1, repeats: true) { [weak self] _ in
                MainActor.assumeIsolated { self?.check() }
            }
            RunLoop.main.add(timer, forMode: .common)
            self.timer = timer
        }
    }

    @discardableResult
    public func receive(_ event: CoreEvent) -> Bool {
        guard !stopped, event.event == "lease_state" else { return false }
        let current = now()
        guard let confirmed = event.confirmedAt else { suspend(); return false }
        // Old buffered lines are harmless, including ones that have since
        // gone stale. They must neither resume nor interrupt a newer lease.
        if let lastConfirmation, confirmed <= lastConfirmation { return false }
        if let resumeAfter, confirmed < resumeAfter { return false }
        guard
              confirmed <= current.addingTimeInterval(30),
              confirmed.addingTimeInterval(lifetime) > current else {
            suspend()
            return false
        }
        lastConfirmation = confirmed
        let next: Target
        var until = min(confirmed, current).addingTimeInterval(lifetime)
        switch event.state {
        case "active":
            guard let id = event.lease, !id.isEmpty, let ends = event.endsAt, ends > current else {
                suspend()
                return false
            }
            next = .lease(id)
            until = min(until, ends)
        case "idle":
            guard event.lease == nil, event.endsAt == nil else { suspend(); return false }
            next = .idle
        default:
            suspend()
            return false
        }
        deadline = until
        // A backward host clock correction must not extend the amount of
        // runtime authorized by this snapshot. Sleep revokes separately.
        monotonicDeadline = uptime() + until.timeIntervalSince(current)
        renterKeys = next == .idle ? [] : event.sshKeys ?? []
        if target == next {
            machine.authorize(renterKeys: renterKeys)
            return true // Renewal: same guest and disk.
        }
        target = next
        revision += 1
        let mine = revision
        machine.interrupt()
        Task { [weak self] in
            guard let self, !stopped, revision == mine else { return }
            // The platform holds no lease for this node: the lease ended
            // (one past its end but not yet settled is still reported), so
            // nothing of the last renter's may stay on the owner's Mac.
            if next == .idle {
                await machine.discard()
                return
            }
            await machine.suspend()
            guard !stopped, revision == mine, target == next else { return }
            check()
            guard revision == mine else { return }
            machine.authorize(renterKeys: renterKeys)
            if case .lease(let id) = next { await machine.run(leaseID: id) }
        }
        return true
    }

    /// Used on core exit, sleep, or isolation. Do not erase the disk: the
    /// platform may confirm that the same paid lease is still in force.
    public func suspend() {
        resumeAfter = now()
        target = nil
        deadline = nil
        monotonicDeadline = nil
        renterKeys = []
        revision += 1
        machine.interrupt()
        let mine = revision
        Task { [weak self] in
            guard let self, revision == mine else { return }
            await machine.suspend()
        }
    }

    func check() {
        if let deadline, let monotonicDeadline,
           now() >= deadline || uptime() >= monotonicDeadline { suspend() }
    }

    public func stop() async {
        stopped = true
        timer?.invalidate()
        timer = nil
        target = nil
        deadline = nil
        monotonicDeadline = nil
        revision += 1
        machine.interrupt()
        await machine.powerOff()
    }

    deinit { timer?.invalidate() }
}
