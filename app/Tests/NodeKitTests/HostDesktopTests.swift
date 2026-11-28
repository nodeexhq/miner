import CommonCrypto
import Darwin
import XCTest
@testable import NodeKit

/// The host's VNC server is private API; these tests check that what the
/// code relies on is there on this macOS, and run the server itself (with
/// no machine behind it) to see it ask for the password and listen only on
/// loopback.
@MainActor
final class HostDesktopTests: XCTestCase {
    func testPasswordsAreEightUnambiguousCharacters() {
        var seen = Set<String>()
        for _ in 0..<200 {
            let p = HostDesktop.newPassword()
            XCTAssertEqual(p.count, 8)
            XCTAssertNil(p.rangeOfCharacter(from: CharacterSet(charactersIn: "0O1lIio")))
            seen.insert(p)
        }
        XCTAssertEqual(seen.count, 200)
    }

    func testPasswordIsKeptWithTheMachine() throws {
        let dir = FileManager.default.temporaryDirectory.appendingPathComponent("host-desktop-\(UUID().uuidString)")
        try FileManager.default.createDirectory(at: dir, withIntermediateDirectories: true)
        defer { try? FileManager.default.removeItem(at: dir) }
        let bundle = VMBundle(directory: dir)
        let first = try HostDesktop.password(for: bundle)
        XCTAssertEqual(try HostDesktop.password(for: bundle), first)
        let mode = try FileManager.default.attributesOfItem(atPath: bundle.desktopPassword.path)[.posixPermissions] as? Int
        XCTAssertEqual(mode, 0o600)
    }

    func testServerAsksForThePasswordOnLoopbackOnly() async throws {
        if let why = HostDesktop.unavailableReason { throw XCTSkip(why) }
        let port = UInt16.random(in: 40000...49000)
        let password = HostDesktop.newPassword()
        let desktop = try await HostDesktop.start(.init(port: port, password: password), machine: nil, timeout: 2)
        defer { desktop.stop() }
        XCTAssertEqual(desktop.port, port)
        // The listening socket is bound to lo0.
        XCTAssertGreaterThan(HostDesktop.restrictToLoopback(port: port), 0)

        let result = try await Task.detached { try Self.vncLogin(port: port, password: password) }.value
        XCTAssertEqual(result.types, [2], "only VNC authentication is offered")
        XCTAssertTrue(result.accepted)
        let wrong = try await Task.detached { try Self.vncLogin(port: port, password: "wrong-pw") }.value
        XCTAssertFalse(wrong.accepted)
    }

    func testBusyPortIsReported() async throws {
        if let why = HostDesktop.unavailableReason { throw XCTSkip(why) }
        let holder = socket(AF_INET, SOCK_STREAM, 0)
        defer { close(holder) }
        var addr = sockaddr_in()
        addr.sin_family = sa_family_t(AF_INET)
        addr.sin_addr.s_addr = inet_addr("127.0.0.1")
        addr.sin_port = 0
        _ = withUnsafePointer(to: &addr) { $0.withMemoryRebound(to: sockaddr.self, capacity: 1) { Darwin.bind(holder, $0, socklen_t(MemoryLayout<sockaddr_in>.size)) } }
        Darwin.listen(holder, 1)
        var bound = sockaddr_storage()
        var length = socklen_t(MemoryLayout<sockaddr_storage>.size)
        _ = withUnsafeMutablePointer(to: &bound) { $0.withMemoryRebound(to: sockaddr.self, capacity: 1) { getsockname(holder, $0, &length) } }
        let port = try XCTUnwrap(HostDesktop.localPort(bound))
        do {
            let desktop = try await HostDesktop.start(.init(port: port, password: "abcdefgh"), machine: nil, timeout: 0.5)
            desktop.stop()
            XCTFail("started on a port in use")
        } catch HostDesktop.Failure.notStarted {}
    }

    /// The client half of RFB 3.8 up to the security result.
    nonisolated static func vncLogin(port: UInt16, password: String) throws -> (types: [UInt8], accepted: Bool) {
        let fd = socket(AF_INET, SOCK_STREAM, 0)
        defer { close(fd) }
        var timeout = timeval(tv_sec: 5, tv_usec: 0)
        setsockopt(fd, SOL_SOCKET, SO_RCVTIMEO, &timeout, socklen_t(MemoryLayout<timeval>.size))
        var addr = sockaddr_in()
        addr.sin_family = sa_family_t(AF_INET)
        addr.sin_addr.s_addr = inet_addr("127.0.0.1")
        addr.sin_port = port.bigEndian
        let ok = withUnsafePointer(to: &addr) { $0.withMemoryRebound(to: sockaddr.self, capacity: 1) { Darwin.connect(fd, $0, socklen_t(MemoryLayout<sockaddr_in>.size)) } }
        guard ok == 0 else { throw POSIXError(.ECONNREFUSED) }
        func read(_ n: Int) -> [UInt8]? {
            var out = [UInt8]()
            while out.count < n {
                var buf = [UInt8](repeating: 0, count: n - out.count)
                let got = recv(fd, &buf, buf.count, 0)
                if got <= 0 { return nil }
                out += buf[0..<got]
            }
            return out
        }
        func write(_ bytes: [UInt8]) { _ = bytes.withUnsafeBytes { send(fd, $0.baseAddress, bytes.count, 0) } }
        guard let version = read(12), String(decoding: version, as: UTF8.self).hasPrefix("RFB 003.") else { throw POSIXError(.EPROTO) }
        write(Array("RFB 003.008\n".utf8))
        guard let count = read(1), let types = read(Int(count[0])) else { throw POSIXError(.EPROTO) }
        guard types.contains(2) else { return (types, false) }
        write([2])
        guard let challenge = read(16) else { throw POSIXError(.EPROTO) }
        write(desResponse(password: password, challenge: challenge))
        // A wrong password closes the connection without a result.
        guard let result = read(4) else { return (types, false) }
        return (types, result == [0, 0, 0, 0])
    }

    /// VNC authentication: the challenge encrypted with DES, keyed by the
    /// first eight bytes of the password with each byte's bits reversed.
    nonisolated static func desResponse(password: String, challenge: [UInt8]) -> [UInt8] {
        var key = [UInt8](repeating: 0, count: 8)
        for (i, b) in password.utf8.prefix(8).enumerated() {
            var r: UInt8 = 0
            for bit in 0..<8 { r = r << 1 | (b >> bit) & 1 }
            key[i] = r
        }
        var out = [UInt8](repeating: 0, count: 16)
        var moved = 0
        CCCrypt(CCOperation(kCCEncrypt), CCAlgorithm(kCCAlgorithmDES), CCOptions(kCCOptionECBMode), key, 8, nil, challenge, 16, &out, 16, &moved)
        return out
    }
}
