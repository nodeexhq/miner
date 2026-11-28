import XCTest
@testable import NodeKit

final class NodeKitTests: XCTestCase {
    /// The core's events arrive as JSON lines and its log separately; a
    /// core that exits is started again.
    func testSupervisorReadsEventsAndRestarts() throws {
        let dir = FileManager.default.temporaryDirectory.appendingPathComponent("nodekit-\(UUID().uuidString)")
        try FileManager.default.createDirectory(at: dir, withIntermediateDirectories: true)
        defer { try? FileManager.default.removeItem(at: dir) }
        let fake = dir.appendingPathComponent("fake-core")
        try """
        #!/bin/sh
        echo "args: $*" >&2
        echo '{"event":"node","state":"active","node":"n1"}'
        echo '{"event":"gate","state":"open","addr":"100.64.0.3"}'
        echo '{"event":"lease_state","state":"active","lease":"l1","ends_at":"2026-11-14T10:00:00.123456Z","confirmed_at":"2026-11-14T09:00:00.123456Z","ssh_keys":["key-1"]}'
        echo '{"event":"something_new","extra":1}'
        echo 'not json'
        echo '{"event":"lease_ended","lease":"l1"}'
        exit 3
        """.write(to: fake, atomically: true, encoding: .utf8)
        try FileManager.default.setAttributes([.posixPermissions: 0o755], ofItemAtPath: fake.path)

        let supervisor = CoreSupervisor(options: .init(binary: fake, stateDirectory: dir, sshTarget: "127.0.0.1:1", vncTarget: "127.0.0.1:2", machine: MachineSize(cpuCount: 8, memoryGB: 12, diskGB: 140)))
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
        // The core's output is read as it arrives, on another thread: the
        // second start can be seen before the first run's last lines are.
        let deadline = Date().addingTimeInterval(10)
        while Date() < deadline {
            lock.lock()
            let ready = events.count >= 5 && logs.contains { $0.contains("exited with status 3") }
            lock.unlock()
            if ready { break }
            Thread.sleep(forTimeInterval: 0.05)
        }
        supervisor.stop()

        lock.lock()
        defer { lock.unlock() }
        let first = Array(events.prefix(5))
        XCTAssertEqual(first.map(\.event), ["node", "gate", "lease_state", "something_new", "lease_ended"])
        XCTAssertEqual(first[0].state, "active")
        XCTAssertEqual(first[1].addr, "100.64.0.3")
        XCTAssertEqual(first[2].lease, "l1")
        XCTAssertNotNil(first[2].endsAt)
        XCTAssertNotNil(first[2].confirmedAt)
        XCTAssertEqual(first[2].sshKeys, ["key-1"])
        XCTAssertTrue(logs.contains { $0.contains("run -events -ssh-target 127.0.0.1:1 -vnc-target 127.0.0.1:2 -dir") }, "\(logs)")
        XCTAssertTrue(logs.contains { $0.hasSuffix("-machine-cpus 8 -machine-memory-gb 12 -machine-disk-gb 140") }, "\(logs)")
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

    /// The obfuscation loginwindow reads: the password's bytes and a zero,
    /// padded with zeros to twelve, XORed with the fixed key.
    func testKCPassword() {
        // "password": 8 bytes, a zero and three of padding.
        XCTAssertEqual(Provisioning.kcpassword("password").map { String(format: "%02x", $0) }.joined(), "0de82150a5d3af8ea3b91f7d")
        // A password of exactly twelve characters still ends with a zero,
        // so it takes a second block.
        XCTAssertEqual(Provisioning.kcpassword("abcdefghjkmn").count, 24)
        // The account's passwords are 23 characters: with the zero, 24 bytes.
        XCTAssertEqual(Provisioning.kcpassword(Provisioning.newAccountPassword()).count, 24)
    }

    func testAccountPasswordIsTypeable() {
        let p = Provisioning.newAccountPassword()
        XCTAssertEqual(p.count, 23)
        XCTAssertEqual(p.split(separator: "-").map(\.count), [5, 5, 5, 5])
        XCTAssertTrue(p.allSatisfy { "abcdefghjkmnpqrstuvwxyz23456789-".contains($0) })
        XCTAssertNotEqual(p, Provisioning.newAccountPassword())
    }

    /// The renter's account logs itself in, has its password where Connect
    /// reads it, and skips Setup Assistant; the password reaches the script
    /// on standard input only.
    func testRenterAccountScript() throws {
        let s = Provisioning.renterAccountScript
        for step in ["IFS= read -r p", "IFS= read -r kc", "sysadminctl -addUser renter", "base64 -D",
                     "autoLoginUser renter", "-m 600 /dev/null /Users/renter/.nvto/account-password",
                     "-m 600 /dev/null /etc/kcpassword", "DidSeeCloudSetup -bool true", "LastSeenBuddyBuildVersion -string \"$b\"",
                     "echo made", "find /Users/Shared/NVTO -maxdepth 1 -name '*.pkg'"] {
            XCTAssertTrue(s.contains(step), step)
        }
        // Both lines are read before the early exit, so ssh's input is always taken.
        let read = try XCTUnwrap(s.range(of: "read -r kc")), exits = try XCTUnwrap(s.range(of: "exit 0"))
        XCTAssertTrue(read.lowerBound < exits.lowerBound)
        XCTAssertFalse(s.contains("set -x"))
        let file = FileManager.default.temporaryDirectory.appendingPathComponent("renter-\(UUID().uuidString).sh")
        defer { try? FileManager.default.removeItem(at: file) }
        try s.write(to: file, atomically: true, encoding: .utf8)
        // ssh runs it with the guest account's shell, which is zsh.
        for shell in ["/bin/sh", "/bin/zsh"] {
            let sh = Process()
            sh.executableURL = URL(fileURLWithPath: shell)
            sh.arguments = ["-n", file.path]
            try sh.run()
            sh.waitUntilExit()
            XCTAssertEqual(sh.terminationStatus, 0, shell)
        }
    }
}

final class CoreCLITests: XCTestCase {
    /// A refusal shows the requirements that were not met, not the core's
    /// closing line.
    func testRefusalNamesTheReasons() {
        let refused = CoreCLI.Result(status: 1, output: "this machine: MacBook Pro\ndoes NOT meet the minimum requirements:\n  - laptops are not accepted\n  - at least 100 GB of free disk is needed\n", errors: "nvto-node: not enrolling\n")
        XCTAssertEqual(refused.message, "This Mac cannot be a node:\n• laptops are not accepted\n• at least 100 GB of free disk is needed")
        let other = CoreCLI.Result(status: 1, output: "this machine: Mac mini\nmeets the minimum requirements to be an NVTO node\n", errors: "nvto-node: POST /v1/nodes/enroll: this enroll code has been used (token_used)\n")
        XCTAssertEqual(other.message, "POST /v1/nodes/enroll: this enroll code has been used (token_used)")
    }
}

final class DiskSizingTests: XCTestCase {
    /// The machine's disk is what the volume can spare after the host's
    /// reserve and room for guest updates (the guest's system is shared with
    /// the base), so a full guest disk never fills the host's.
    func testDiskFollowsFreeSpace() {
        XCTAssertNil(VMResources.diskGB(forFreeGB: 40))
        XCTAssertNil(VMResources.diskGB(forFreeGB: 55))
        XCTAssertNil(VMResources.diskGB(forFreeGB: 119), "79 GB would be left: below the minimum")
        XCTAssertEqual(VMResources.diskGB(forFreeGB: 120), 80)
        XCTAssertEqual(VMResources.diskGB(forFreeGB: 173), 130, "the 256 GB Mac mini in testing")
        XCTAssertEqual(VMResources.diskGB(forFreeGB: 200), 160, "a new 256 GB Mac mini")
        XCTAssertEqual(VMResources.diskGB(forFreeGB: 460), 420, "a 512 GB Mac")
        XCTAssertEqual(VMResources.diskGB(forFreeGB: 1900), 500, "capped")
        for free in stride(from: UInt64(120), through: 2000, by: 7) {
            let disk = VMResources.diskGB(forFreeGB: free)!
            // The base's system is shared with the machine, so base plus a
            // full machine take about the machine's size.
            XCTAssertLessThanOrEqual(disk + VMResources.hostReserveGB + VMResources.guestUpdateGB, free)
        }
    }
}

final class FirstRunScriptTests: XCTestCase {
    func testScriptMakesTheAccountOnceAndRemovesItsTrigger() {
        let s = Provisioning.firstRunScript
        XCTAssertTrue(s.hasPrefix("#!/bin/sh\n"))
        XCTAssertTrue(s.contains("U=nvto\n"))
        for step in ["sysadminctl -addUser $U", "openssl rand", "system/com.openssh.sshd", "system/com.apple.screensharing",
                     ".AppleSetupDone", "com.apple.access_ssh", "-a $U -t user", "IsHidden 1", "pmset -a sleep 0", ".nvto-firstboot-ok", "checkmember -m $U", "AutomaticDownload -bool false", "shutdown -h now",
                     "D=/Users/Shared/NVTO\n", "2HCKV38EEC", "PU9BNSBJW7", "spctl -a -t install"] {
            XCTAssertTrue(s.contains(step), step)
        }
        XCTAssertFalse(s.contains("    #!/"), "the script must not be indented")
        // The password is generated before commands are echoed to the log.
        let made = try! XCTUnwrap(s.range(of: "openssl rand")), echoed = try! XCTUnwrap(s.range(of: "set -x"))
        XCTAssertTrue(made.lowerBound < echoed.lowerBound)
    }

    func testScriptIsValidShell() throws {
        let file = FileManager.default.temporaryDirectory.appendingPathComponent("firstrun-\(UUID().uuidString).sh")
        defer { try? FileManager.default.removeItem(at: file) }
        try Provisioning.firstRunScript.write(to: file, atomically: true, encoding: .utf8)
        let sh = Process()
        sh.executableURL = URL(fileURLWithPath: "/bin/sh")
        sh.arguments = ["-n", file.path]
        try sh.run()
        sh.waitUntilExit()
        XCTAssertEqual(sh.terminationStatus, 0)
    }
}
