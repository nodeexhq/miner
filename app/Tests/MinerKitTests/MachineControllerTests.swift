import XCTest
@testable import MinerKit

/// Exercise the real controller and disk transitions, replacing only the
/// Virtualization.framework runner so startup and power-off can be blocked.
@MainActor
final class MachineControllerTests: XCTestCase {
    private func store() throws -> VMStore {
        let root = FileManager.default.temporaryDirectory.appendingPathComponent("lease-controller-\(UUID().uuidString)")
        let store = VMStore(root: root)
        try FileManager.default.createDirectory(at: store.base.directory, withIntermediateDirectories: true)
        try Data("clean base".utf8).write(to: store.base.disk)
        addTeardownBlock { try? FileManager.default.removeItem(at: root) }
        return store
    }

    private func settle() async { for _ in 0..<100 { await Task.yield() } }

    func testRevocationDuringStartCutsNetworkAndCannotResurrect() async throws {
        let store = try store()
        let guest = ControlledVM()
        guest.holdStart = true
        var creations = 0
        let controller = MachineController(store: store, resources: .forThisHost()) { _, _, _ in
            creations += 1
            return guest
        }
        let boot = Task { await controller.run(leaseID: "lease-1") }
        await settle()
        XCTAssertTrue(guest.startPending)
        let obsoleteCallback = guest.onStop
        controller.interrupt()
        XCTAssertFalse(guest.networkConnected)
        let stop = Task { await controller.suspend() }
        await settle()
        XCTAssertFalse(guest.stopped, "power-off is waiting for startup")
        guest.finishStart()
        await boot.value
        await stop.value
        obsoleteCallback?(nil)
        await settle()
        XCTAssertTrue(guest.stopped)
        XCTAssertFalse(guest.networkConnected)
        XCTAssertEqual(creations, 1)
        XCTAssertEqual(controller.state, .suspended)
    }

    func testRebootStopCannotBeOvertakenByRevocationAndAnotherLease() async throws {
        let store = try store()
        var guests: [ControlledVM] = []
        let controller = MachineController(store: store, resources: .forThisHost()) { _, _, _ in
            let guest = ControlledVM()
            guests.append(guest)
            return guest
        }
        await controller.run(leaseID: "lease-1")
        try Data("old work and remote login".utf8).write(to: store.current.disk)
        let old = try XCTUnwrap(guests.first)
        old.holdStop = true
        // A guest shutdown schedules the controller's automatic reboot.
        old.onStop?(nil)
        await settle()
        XCTAssertTrue(old.stopPending)
        XCTAssertFalse(old.networkConnected)
        controller.interrupt()
        let revoke = Task { await controller.suspend() }
        await settle()
        let next = Task { await controller.run(leaseID: "lease-2") }
        await settle()
        XCTAssertEqual(guests.count, 1, "detaching the runner must not hide its pending power-off")
        XCTAssertEqual(store.currentLease, "lease-1")
        XCTAssertEqual(try String(contentsOf: store.current.disk, encoding: .utf8), "old work and remote login")
        old.finishStop()
        await revoke.value
        await next.value
        XCTAssertTrue(old.stopped)
        XCTAssertEqual(guests.count, 2)
        XCTAssertEqual(store.currentLease, "lease-2")
        XCTAssertEqual(try String(contentsOf: store.current.disk, encoding: .utf8), "clean base")
        await controller.suspend()
    }

    func testSameLeaseRebootKeepsDiskAndOwnership() async throws {
        let store = try store()
        var guests: [ControlledVM] = []
        let controller = MachineController(store: store, resources: .forThisHost()) { _, _, _ in
            let guest = ControlledVM()
            guests.append(guest)
            return guest
        }
        await controller.run(leaseID: "lease-1")
        try Data("working tree".utf8).write(to: store.current.disk)
        let old = try XCTUnwrap(guests.first)
        old.onStop?(nil)
        await settle()
        XCTAssertEqual(guests.count, 2)
        XCTAssertTrue(old.stopped)
        XCTAssertEqual(store.currentLease, "lease-1")
        XCTAssertEqual(try String(contentsOf: store.current.disk, encoding: .utf8), "working tree")
        await controller.suspend()
    }

    func testQueuedGuestAndNetworkRetriesCannotRunAfterRevocation() async throws {
        let store = try store()
        let guest = ControlledVM()
        var networkExit: (@Sendable () -> Void)?
        var creations = 0
        let controller = MachineController(store: store, resources: .forThisHost()) { _, _, onNetworkExit in
            creations += 1
            networkExit = onNetworkExit
            return guest
        }
        await controller.run(leaseID: "lease-1")
        guest.onStop?(nil)
        networkExit?()
        // Both callbacks have queued work, but neither may start a guest
        // after the synchronous revocation advances the generation.
        controller.interrupt()
        await controller.suspend()
        await settle()
        XCTAssertTrue(guest.stopped)
        XCTAssertFalse(guest.networkConnected)
        XCTAssertEqual(creations, 1)
        XCTAssertEqual(controller.state, .suspended)
    }

    func testFailedOwnershipWriteCannotBootAnUnmarkedDisk() async throws {
        let store = try store()
        // A directory at the marker path makes its atomic write fail.
        try FileManager.default.createDirectory(at: store.base.directory.appendingPathComponent("lease"), withIntermediateDirectories: true)
        var creations = 0
        let controller = MachineController(store: store, resources: .forThisHost()) { _, _, _ in
            creations += 1
            return ControlledVM()
        }
        await controller.run(leaseID: "lease-1")
        XCTAssertEqual(creations, 0)
        guard case .failed = controller.state else { return XCTFail("ownership failure must stop boot") }
        await controller.suspend()
    }
}

@MainActor
private final class ControlledVM: VMRuntime {
    var onStop: ((Error?) -> Void)?
    var networkConnected = true
    var stopped = false
    var holdStart = false
    var holdStop = false
    var startPending: Bool { startWaiter != nil }
    var stopPending: Bool { !stopWaiters.isEmpty }
    private var startWaiter: CheckedContinuation<Void, Never>?
    private var stopWaiters: [CheckedContinuation<Void, Never>] = []

    func disconnect() { networkConnected = false }
    func start() async throws {
        if holdStart { await withCheckedContinuation { startWaiter = $0 } }
    }
    func stop() async {
        disconnect()
        // Like VMRunner, power-off waits for startup to finish.
        while startPending { await Task.yield() }
        if holdStop { await withCheckedContinuation { stopWaiters.append($0) } }
        stopped = true
    }
    func sshEndpoint(timeout: TimeInterval) async throws -> VMRunner.Endpoint {
        VMRunner.Endpoint(host: "127.0.0.1", port: MachineController.sshPort)
    }
    func finishStart() {
        holdStart = false
        let waiter = startWaiter
        startWaiter = nil
        waiter?.resume()
    }
    func finishStop() {
        holdStop = false
        let waiters = stopWaiters
        stopWaiters = []
        waiters.forEach { $0.resume() }
    }
}
