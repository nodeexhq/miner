import XCTest
@testable import NodeKit

/// Tests exercise power/network effects without booting a VM or touching the
/// owner's real machines. The same coordinator drives MachineController.
@MainActor
final class LeaseSupervisorTests: XCTestCase {
    private var clock = Date(timeIntervalSince1970: 1_800_000_000)

    private func snapshot(_ id: String? = "lease-1", end: Date? = nil,
                          confirmed: Date? = nil, keys: [String] = []) throws -> CoreEvent {
        var wire: [String: Any] = ["event": "lease_state", "state": id == nil ? "idle" : "active",
                                   "confirmed_at": (confirmed ?? clock).timeIntervalSince1970,
                                   "ssh_keys": keys]
        if let id { wire["lease"] = id; wire["ends_at"] = (end ?? clock.addingTimeInterval(120)).timeIntervalSince1970 }
        let decoder = JSONDecoder()
        decoder.dateDecodingStrategy = .secondsSince1970
        return try decoder.decode(CoreEvent.self, from: JSONSerialization.data(withJSONObject: wire))
    }

    private func settle() async { for _ in 0..<30 { await Task.yield() } }

    func testRestartNeverBootsAnOldDiskBeforeConfirmation() async throws {
        let machine = FakeLeaseMachine(lease: "lease-1", disk: "renter's files and Jump login")
        let supervisor = LeaseSupervisor(machine: machine, now: { self.clock }, automaticallyCheck: false)
        await settle()
        XCTAssertFalse(machine.online)
        XCTAssertEqual(machine.starts, [])
        XCTAssertEqual(machine.disk, "renter's files and Jump login")
        supervisor.receive(try snapshot())
        await settle()
        XCTAssertTrue(machine.online)
        XCTAssertEqual(machine.disk, "renter's files and Jump login")
        await supervisor.stop()
    }

    func testEndedLeaseDestroysTheGuestAndNewLeaseGetsAFreshOne() async throws {
        let machine = FakeLeaseMachine()
        let supervisor = LeaseSupervisor(machine: machine, now: { self.clock }, automaticallyCheck: false)
        supervisor.receive(try snapshot(keys: ["key-1"]))
        await settle()
        machine.disk = "Jump, UU, renter data"
        clock.addTimeInterval(15)
        supervisor.receive(try snapshot(nil))
        XCTAssertFalse(machine.online, "network revocation is synchronous, before power-off awaits")
        await settle()
        // The platform reports a lease until it is settled, so none means
        // ended: nothing of the renter's stays on the owner's Mac.
        XCTAssertNil(machine.lease)
        XCTAssertEqual(machine.disk, "destroyed")
        XCTAssertEqual(machine.keys, [])
        XCTAssertFalse(machine.online, "lease:null never authorizes an idle boot")
        clock.addTimeInterval(15)
        supervisor.receive(try snapshot("lease-2"))
        await settle()
        XCTAssertTrue(machine.online)
        XCTAssertEqual(machine.disk, "clean base")
        await supervisor.stop()
    }

    func testDeadlineStopsTheGuestWithoutAnyMoreCoreOrPlatformEvents() async throws {
        let machine = FakeLeaseMachine()
        let supervisor = LeaseSupervisor(machine: machine, now: { self.clock }, automaticallyCheck: false)
        supervisor.receive(try snapshot(end: clock.addingTimeInterval(30)))
        await settle()
        machine.disk = "keep for a possible confirmed renewal"
        clock.addTimeInterval(30)
        supervisor.check()
        XCTAssertFalse(machine.online)
        await settle()
        XCTAssertFalse(machine.online)
        XCTAssertEqual(machine.disk, "keep for a possible confirmed renewal")
        await supervisor.stop()
    }

    func testRenewalUpdatesDeadlineWithoutRestartOrReset() async throws {
        let machine = FakeLeaseMachine()
        let supervisor = LeaseSupervisor(machine: machine, now: { self.clock }, automaticallyCheck: false)
        supervisor.receive(try snapshot(end: clock.addingTimeInterval(30)))
        await settle()
        machine.disk = "working tree"
        clock.addTimeInterval(15)
        supervisor.receive(try snapshot(end: clock.addingTimeInterval(120), keys: ["new-key"]))
        clock.addTimeInterval(15)
        supervisor.check()
        await settle()
        XCTAssertTrue(machine.online)
        XCTAssertEqual(machine.starts, ["lease-1"])
        XCTAssertEqual(machine.disk, "working tree")
        XCTAssertEqual(machine.keys, ["new-key"])
        clock.addTimeInterval(105)
        supervisor.check()
        XCTAssertFalse(machine.online)
        await supervisor.stop()
    }

    func testMissingHeartbeatsStopLongLeasesAndAFreshConfirmationResumesSameDisk() async throws {
        let machine = FakeLeaseMachine()
        let supervisor = LeaseSupervisor(machine: machine, now: { self.clock }, automaticallyCheck: false)
        let end = clock.addingTimeInterval(3600)
        supervisor.receive(try snapshot(end: end))
        await settle()
        machine.disk = "unfinished AI work"
        clock.addTimeInterval(180)
        supervisor.check()
        XCTAssertFalse(machine.online)
        await settle()
        clock.addTimeInterval(1)
        supervisor.receive(try snapshot(end: end))
        await settle()
        XCTAssertTrue(machine.online)
        XCTAssertEqual(machine.disk, "unfinished AI work")
        await supervisor.stop()
    }

    func testCoreExitRevokesImmediatelyAndOldSnapshotCannotRestart() async throws {
        let machine = FakeLeaseMachine()
        let supervisor = LeaseSupervisor(machine: machine, now: { self.clock }, automaticallyCheck: false)
        let old = try snapshot()
        supervisor.receive(old)
        await settle()
        supervisor.suspend()
        XCTAssertFalse(machine.online)
        supervisor.receive(old)
        await settle()
        XCTAssertFalse(machine.online)
        clock.addTimeInterval(1)
        supervisor.receive(try snapshot())
        await settle()
        XCTAssertTrue(machine.online)
        await supervisor.stop()
    }

    func testOnlyAConfirmationAfterSuspensionCanResume() async throws {
        let machine = FakeLeaseMachine()
        let supervisor = LeaseSupervisor(machine: machine, now: { self.clock }, automaticallyCheck: false)
        supervisor.receive(try snapshot())
        await settle()
        let buffered = try snapshot(confirmed: clock.addingTimeInterval(3))
        clock.addTimeInterval(5)
        supervisor.suspend()
        XCTAssertFalse(supervisor.receive(buffered))
        await settle()
        XCTAssertFalse(machine.online)
        clock.addTimeInterval(1)
        XCTAssertTrue(supervisor.receive(try snapshot()))
        await settle()
        XCTAssertTrue(machine.online)
        await supervisor.stop()
    }

    func testOldBufferedSnapshotCannotInterruptANewerRenewal() async throws {
        let machine = FakeLeaseMachine()
        let supervisor = LeaseSupervisor(machine: machine, now: { self.clock }, automaticallyCheck: false)
        let old = try snapshot()
        supervisor.receive(old)
        await settle()
        clock.addTimeInterval(170)
        supervisor.receive(try snapshot())
        clock.addTimeInterval(20)
        XCTAssertFalse(supervisor.receive(old))
        supervisor.check()
        await settle()
        XCTAssertTrue(machine.online)
        XCTAssertEqual(machine.starts, ["lease-1"])
        await supervisor.stop()
    }

    func testTimerEnforcesDeadlineWithoutEventDelivery() async throws {
        let machine = FakeLeaseMachine()
        let supervisor = LeaseSupervisor(machine: machine, now: { self.clock })
        supervisor.receive(try snapshot(end: clock.addingTimeInterval(1)))
        await settle()
        XCTAssertTrue(machine.online)
        let revoked = expectation(description: "timer cuts the network")
        machine.onInterrupt = { revoked.fulfill() }
        clock.addTimeInterval(1)
        await fulfillment(of: [revoked], timeout: 3)
        machine.onInterrupt = nil
        XCTAssertFalse(machine.online)
        await supervisor.stop()
    }

    func testBackwardHostClockCorrectionCannotExtendAuthorization() async throws {
        let machine = FakeLeaseMachine()
        var elapsed: TimeInterval = 0
        let supervisor = LeaseSupervisor(machine: machine, now: { self.clock }, uptime: { elapsed }, automaticallyCheck: false)
        supervisor.receive(try snapshot(end: clock.addingTimeInterval(30)))
        await settle()
        XCTAssertTrue(machine.online)
        clock.addTimeInterval(-3600)
        elapsed = 30
        supervisor.check()
        XCTAssertFalse(machine.online)
        await supervisor.stop()
    }

    func testExpiredDiskIsNotResumedAfterRestartAndNewTenantGetsCleanDisk() async throws {
        let machine = FakeLeaseMachine(lease: "lease-1", disk: "old credentials")
        let supervisor = LeaseSupervisor(machine: machine, now: { self.clock }, automaticallyCheck: false)
        supervisor.receive(try snapshot(end: clock.addingTimeInterval(-1)))
        await settle()
        XCTAssertFalse(machine.online)
        clock.addTimeInterval(1)
        supervisor.receive(try snapshot("lease-2"))
        await settle()
        XCTAssertTrue(machine.online)
        XCTAssertEqual(machine.lease, "lease-2")
        XCTAssertEqual(machine.disk, "clean base")
        await supervisor.stop()
    }

    func testIdleConfirmationAfterRestartDestroysPreviousTenantDisk() async throws {
        // The lease ended while the app was not running.
        let machine = FakeLeaseMachine(lease: "lease-1", disk: "old credentials")
        let supervisor = LeaseSupervisor(machine: machine, now: { self.clock }, automaticallyCheck: false)
        supervisor.receive(try snapshot(nil))
        await settle()
        XCTAssertNil(machine.lease)
        XCTAssertEqual(machine.disk, "destroyed")
        XCTAssertFalse(machine.online)
        await supervisor.stop()
    }

    func testQueuedStartCannotRunAfterTerminationOrStop() async throws {
        let machine = FakeLeaseMachine()
        let supervisor = LeaseSupervisor(machine: machine, now: { self.clock }, automaticallyCheck: false)
        supervisor.receive(try snapshot())
        clock.addTimeInterval(1)
        supervisor.receive(try snapshot(nil))
        await settle()
        XCTAssertEqual(machine.starts, [])
        clock.addTimeInterval(1)
        supervisor.receive(try snapshot("lease-2"))
        await supervisor.stop()
        await settle()
        XCTAssertFalse(machine.online)
        XCTAssertEqual(machine.starts, [])
        XCTAssertFalse(supervisor.receive(try snapshot("lease-3")))
    }

    func testDeadlineWhilePowerOffIsPendingDoesNotStartAnotherGuest() async throws {
        let machine = FakeLeaseMachine()
        let supervisor = LeaseSupervisor(machine: machine, now: { self.clock }, automaticallyCheck: false)
        machine.holdStop = true
        supervisor.receive(try snapshot(end: clock.addingTimeInterval(5)))
        await settle()
        clock.addTimeInterval(5)
        supervisor.check()
        machine.releaseStops()
        await settle()
        XCTAssertEqual(machine.starts, [])
        XCTAssertFalse(machine.online)
        await supervisor.stop()
    }

    func testStaleOrMalformedConfirmationsNeverAuthorize() async throws {
        let machine = FakeLeaseMachine()
        let supervisor = LeaseSupervisor(machine: machine, now: { self.clock }, automaticallyCheck: false)
        XCTAssertFalse(supervisor.receive(try snapshot(confirmed: clock.addingTimeInterval(-181))))
        XCTAssertFalse(supervisor.receive(try snapshot(confirmed: clock.addingTimeInterval(31))))
        let missing = try JSONDecoder().decode(CoreEvent.self, from: Data("{\"event\":\"lease_state\",\"state\":\"active\"}".utf8))
        XCTAssertFalse(supervisor.receive(missing))
        await settle()
        XCTAssertEqual(machine.starts, [])
        XCTAssertFalse(machine.online)
        await supervisor.stop()
    }

    func testLateRenewalOfALeasePastItsEndPreservesTheSameDisk() async throws {
        let machine = FakeLeaseMachine()
        let supervisor = LeaseSupervisor(machine: machine, now: { self.clock }, automaticallyCheck: false)
        supervisor.receive(try snapshot(end: clock.addingTimeInterval(5)))
        await settle()
        machine.disk = "unfinished work"
        clock.addTimeInterval(5)
        supervisor.check()
        // Until the platform renews or ends it, it reports the lease with
        // its end in the past: the machine stays off, and kept.
        clock.addTimeInterval(1)
        XCTAssertFalse(supervisor.receive(try snapshot(end: clock.addingTimeInterval(-1))))
        await settle()
        XCTAssertFalse(machine.online)
        XCTAssertEqual(machine.disk, "unfinished work")
        clock.addTimeInterval(15)
        supervisor.receive(try snapshot())
        await settle()
        XCTAssertTrue(machine.online)
        XCTAssertEqual(machine.disk, "unfinished work")
        await supervisor.stop()
    }
}

@MainActor
private final class FakeLeaseMachine: LeaseMachine {
    var lease: String?
    var disk: String
    var online = false
    var starts: [String] = []
    var keys: [String] = []
    var holdStop = false
    var onInterrupt: (() -> Void)?
    private var stops: [CheckedContinuation<Void, Never>] = []

    init(lease: String? = nil, disk: String = "clean base") { self.lease = lease; self.disk = disk }
    func interrupt() { online = false; keys = []; onInterrupt?() }
    func suspend() async {
        online = false
        if holdStop { await withCheckedContinuation { stops.append($0) } }
    }
    func powerOff() async { online = false }
    func discard() async {
        online = false
        if holdStop { await withCheckedContinuation { stops.append($0) } }
        lease = nil
        disk = "destroyed"
    }
    func run(leaseID: String) async {
        if lease != leaseID { disk = "clean base" }
        lease = leaseID
        online = true
        starts.append(leaseID)
    }
    func authorize(renterKeys: [String]) { keys = renterKeys }
    func releaseStops() {
        holdStop = false
        let pending = stops
        stops = []
        pending.forEach { $0.resume() }
    }
}
