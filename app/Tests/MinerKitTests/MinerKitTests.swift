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
}

final class ProvisioningTests: XCTestCase {
    /// Only single-line OpenSSH public keys may reach authorized_keys.
    func testOnlyPublicKeysAreAccepted() {
        XCTAssertTrue(Provisioning.isPublicKey("ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIGk5jv0mY0b8o0Jm1J0cK0v8pT9c2mQk0m0m0m0m0m0m renter@laptop"))
        XCTAssertTrue(Provisioning.isPublicKey("ecdsa-sha2-nistp256 AAAAE2VjZHNh"))
        XCTAssertFalse(Provisioning.isPublicKey("ssh-ed25519"))
        XCTAssertFalse(Provisioning.isPublicKey("ssh-ed25519 not*base64"))
        XCTAssertFalse(Provisioning.isPublicKey("command=\"rm -rf /\" ssh-ed25519 AAAAC3NzaC1lZDI1NTE5"))
        XCTAssertFalse(Provisioning.isPublicKey("ssh-ed25519 AAAAC3NzaC1lZDI1NTE5\nssh-ed25519 AAAAC3NzaC1lZDI1NTE5"))
        XCTAssertFalse(Provisioning.isPublicKey("ssh-dss AAAAB3NzaC1kc3M="))
        XCTAssertFalse(Provisioning.isPublicKey(""))
    }
}

final class CoreCLITests: XCTestCase {
    /// A refusal shows the requirements that were not met, not the core's
    /// closing line.
    func testRefusalNamesTheReasons() {
        let refused = CoreCLI.Result(status: 1, output: "this machine: MacBook Pro\ndoes NOT meet the minimum requirements:\n  - laptops are not accepted\n  - at least 100 GB of free disk is needed\n", errors: "nodeex-miner: not enrolling\n")
        XCTAssertEqual(refused.message, "This Mac cannot be a node:\n• laptops are not accepted\n• at least 100 GB of free disk is needed")
        let other = CoreCLI.Result(status: 1, output: "this machine: Mac mini\nmeets the minimum requirements to be a NodeEx node\n", errors: "nodeex-miner: POST /v1/nodes/enroll: this enroll code has been used (token_used)\n")
        XCTAssertEqual(other.message, "POST /v1/nodes/enroll: this enroll code has been used (token_used)")
    }
}

final class DiskSizingTests: XCTestCase {
    /// The machine's disk is what the volume can spare after the base and
    /// the host's reserve, so a full guest disk never fills the host's.
    func testDiskFollowsFreeSpace() {
        XCTAssertNil(VMResources.diskGB(forFreeGB: 40))
        XCTAssertNil(VMResources.diskGB(forFreeGB: 55))
        XCTAssertNil(VMResources.diskGB(forFreeGB: 134), "79 GB would be left: below the minimum")
        XCTAssertEqual(VMResources.diskGB(forFreeGB: 135), 80)
        XCTAssertEqual(VMResources.diskGB(forFreeGB: 200), 140, "a new 256 GB Mac mini")
        XCTAssertEqual(VMResources.diskGB(forFreeGB: 209), 150)
        XCTAssertEqual(VMResources.diskGB(forFreeGB: 460), 400, "a 512 GB Mac")
        XCTAssertEqual(VMResources.diskGB(forFreeGB: 1900), 500, "capped")
        for free in stride(from: UInt64(135), through: 2000, by: 7) {
            let disk = VMResources.diskGB(forFreeGB: free)!
            XCTAssertLessThanOrEqual(disk + VMResources.baseGB + VMResources.hostReserveGB, free)
        }
    }
}
