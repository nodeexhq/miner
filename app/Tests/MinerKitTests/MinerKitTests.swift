import XCTest
@testable import MinerKit

final class MinerKitTests: XCTestCase {
    /// The core's events arrive as JSON lines and its log separately; a
    /// core that exits is started again.
    func testSupervisorReadsEventsAndRestarts() throws {
        let dir = FileManager.default.temporaryDirectory.appendingPathComponent("minerkit-\(UUID().uuidString)")
        try FileManager.default.createDirectory(at: dir, withIntermediateDirectories: true)
        defer { try? FileManager.default.removeItem(at: dir) }
        let fake = dir.appendingPathComponent("fake-core")
        try """
        #!/bin/sh
        echo "args: $*" >&2
        echo '{"event":"node","state":"active","node":"n1"}'
        echo '{"event":"gate","state":"open","addr":"100.64.0.3"}'
        echo '{"event":"lease_started","lease":"l1","ends_at":"2026-11-14T10:00:00.123456Z"}'
        echo '{"event":"something_new","extra":1}'
        echo 'not json'
        echo '{"event":"lease_ended","lease":"l1"}'
        exit 3
        """.write(to: fake, atomically: true, encoding: .utf8)
        try FileManager.default.setAttributes([.posixPermissions: 0o755], ofItemAtPath: fake.path)

        let supervisor = CoreSupervisor(options: .init(binary: fake, stateDirectory: dir, sshTarget: "127.0.0.1:1", vncTarget: "127.0.0.1:2"))
        let lock = NSLock()
        var events: [CoreEvent] = []
        var logs: [String] = []
        var starts = 0
        let twice = expectation(description: "the core was started a second time")
        supervisor.onEvent = { e in lock.lock(); events.append(e); lock.unlock() }
        supervisor.onLog = { l in lock.lock(); logs.append(l); lock.unlock() }
        supervisor.onRunning = { running in
            guard running else { return }
            lock.lock(); starts += 1; let n = starts; lock.unlock()
            if n == 2 { twice.fulfill() }
        }
        supervisor.start()
        wait(for: [twice], timeout: 15)
        supervisor.stop()

        lock.lock()
        defer { lock.unlock() }
        let first = Array(events.prefix(5))
        XCTAssertEqual(first.map(\.event), ["node", "gate", "lease_started", "something_new", "lease_ended"])
        XCTAssertEqual(first[0].state, "active")
        XCTAssertEqual(first[1].addr, "100.64.0.3")
        XCTAssertEqual(first[2].lease, "l1")
        XCTAssertNotNil(first[2].endsAt)
        XCTAssertTrue(logs.contains { $0.contains("run -events -ssh-target 127.0.0.1:1 -vnc-target 127.0.0.1:2 -dir") }, "\(logs)")
        XCTAssertTrue(logs.contains { $0.contains("exited with status 3") })
    }

    func testLeasedAddressMatchesTheGuestsMAC() {
        let leases = """
        {
        \tname=other
        \tip_address=192.168.64.9
        \thw_address=1,aa:bb:cc:dd:ee:ff
        }
        {
        \tname=guest
        \tip_address=192.168.64.5
        \thw_address=1,6:b:c0:1:2:a3
        \tlease=0x6736f0a1
        }
        {
        \tname=guest-old
        \tip_address=192.168.64.3
        \thw_address=1,6:b:c0:1:2:a3
        }
        """
        let mac = VMRunner.normalize("06:0B:C0:01:02:A3\n")
        XCTAssertEqual(mac, "6:b:c0:1:2:a3")
        XCTAssertEqual(VMRunner.leasedAddress(for: mac, leases: leases), "192.168.64.5")
        XCTAssertNil(VMRunner.leasedAddress(for: VMRunner.normalize("00:00:00:00:00:01"), leases: leases))
    }

    /// A connection to the loopback port reaches the current target, and
    /// stops reaching anything once the target is cleared.
    func testForwarderFollowsItsTarget() throws {
        let echo = try NWEcho()
        let forwarder = try Forwarder(port: 28022)
        defer { forwarder.stop() }
        forwarder.setTarget(host: "127.0.0.1", port: echo.port)
        Thread.sleep(forTimeInterval: 0.3)
        XCTAssertEqual(try roundTrip(port: 28022, send: "hello"), "hello")
        forwarder.setTarget(host: nil, port: 0)
        Thread.sleep(forTimeInterval: 0.3)
        XCTAssertNil(try? roundTrip(port: 28022, send: "hello"))
    }
}

import Network

/// A TCP server on a free loopback port that sends back what it receives.
final class NWEcho {
    let listener: NWListener
    var port: UInt16 { listener.port!.rawValue }

    init() throws {
        listener = try NWListener(using: .tcp, on: .any)
        let ready = DispatchSemaphore(value: 0)
        listener.stateUpdateHandler = { if case .ready = $0 { ready.signal() } }
        listener.newConnectionHandler = { c in
            c.start(queue: .global())
            func loop() {
                c.receive(minimumIncompleteLength: 1, maximumLength: 4096) { data, _, done, _ in
                    if let data, !data.isEmpty { c.send(content: data, completion: .contentProcessed { _ in if !done { loop() } }) } else { c.cancel() }
                }
            }
            loop()
        }
        listener.start(queue: .global())
        _ = ready.wait(timeout: .now() + 5)
    }
}

struct RoundTripFailed: Error {}

func roundTrip(port: UInt16, send text: String) throws -> String {
    let c = NWConnection(host: "127.0.0.1", port: NWEndpoint.Port(rawValue: port)!, using: .tcp)
    let done = DispatchSemaphore(value: 0)
    var reply: String?
    c.start(queue: .global())
    c.send(content: Data(text.utf8), completion: .contentProcessed { _ in })
    c.receive(minimumIncompleteLength: 1, maximumLength: 4096) { data, _, _, _ in
        reply = data.flatMap { String(data: $0, encoding: .utf8) }
        done.signal()
    }
    _ = done.wait(timeout: .now() + 3)
    c.cancel()
    guard let reply, !reply.isEmpty else { throw RoundTripFailed() }
    return reply
}
