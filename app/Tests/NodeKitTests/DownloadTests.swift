import XCTest
@testable import NodeKit

/// Serves a fixed payload and honours Range requests unless told not to.
final class RangeProtocol: URLProtocol {
    nonisolated(unsafe) static var payload = Data()
    nonisolated(unsafe) static var honourRanges = true
    nonisolated(unsafe) static var requests: [String?] = []

    override class func canInit(with request: URLRequest) -> Bool { true }
    override class func canonicalRequest(for request: URLRequest) -> URLRequest { request }

    override func startLoading() {
        let range = request.value(forHTTPHeaderField: "Range")
        Self.requests.append(range)
        var status = 200
        var body = Self.payload
        if Self.honourRanges, let range, range.hasPrefix("bytes="), let from = Int(range.dropFirst(6).dropLast()) {
            if from >= Self.payload.count {
                status = 416
                body = Data()
            } else {
                status = 206
                body = Self.payload.subdata(in: from..<Self.payload.count)
            }
        }
        let response = HTTPURLResponse(url: request.url!, statusCode: status, httpVersion: "HTTP/1.1",
                                       headerFields: ["Content-Length": String(body.count)])!
        client?.urlProtocol(self, didReceive: response, cacheStoragePolicy: .notAllowed)
        // In pieces, as a network would deliver it.
        var i = 0
        while i < body.count {
            client?.urlProtocol(self, didLoad: body.subdata(in: i..<min(i + 4096, body.count)))
            i += 4096
        }
        client?.urlProtocolDidFinishLoading(self)
    }

    override func stopLoading() {}
}

final class DownloadTests: XCTestCase {
    private func config() -> URLSessionConfiguration {
        let c = URLSessionConfiguration.ephemeral
        c.protocolClasses = [RangeProtocol.self]
        return c
    }

    private func run(havePrefix: Int, honour: Bool) async throws -> (Data, [String?]) {
        let dir = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString)
        try FileManager.default.createDirectory(at: dir, withIntermediateDirectories: true)
        defer { try? FileManager.default.removeItem(at: dir) }
        RangeProtocol.payload = Data((0..<100_000).map { UInt8($0 % 251) })
        RangeProtocol.honourRanges = honour
        RangeProtocol.requests = []
        let partial = dir.appendingPathComponent("x.partial"), dest = dir.appendingPathComponent("x")
        if havePrefix > 0 { try RangeProtocol.payload.prefix(havePrefix).write(to: partial) }
        try await ResumableDownload.fetch(URL(string: "https://example.test/x.ipsw")!, to: dest, partial: partial,
                                          configuration: config(), progress: { _ in })
        XCTAssertFalse(FileManager.default.fileExists(atPath: partial.path))
        return (try Data(contentsOf: dest), RangeProtocol.requests)
    }

    func testFreshDownload() async throws {
        let (data, requests) = try await run(havePrefix: 0, honour: true)
        XCTAssertEqual(data, RangeProtocol.payload)
        XCTAssertEqual(requests, [nil])
    }

    func testContinuesAnInterruptedDownload() async throws {
        let (data, requests) = try await run(havePrefix: 40_000, honour: true)
        XCTAssertEqual(data, RangeProtocol.payload)
        XCTAssertEqual(requests, ["bytes=40000-"])
    }

    func testStartsOverWhenTheServerIgnoresTheRange() async throws {
        let (data, _) = try await run(havePrefix: 40_000, honour: false)
        XCTAssertEqual(data, RangeProtocol.payload)
    }

    func testAFinishedPartialFileIsKept() async throws {
        let (data, requests) = try await run(havePrefix: 100_000, honour: true)
        XCTAssertEqual(data, RangeProtocol.payload)
        XCTAssertEqual(requests, ["bytes=100000-"])
    }

    func testFindsImagesTheOwnerHas() throws {
        let dir = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString)
        try FileManager.default.createDirectory(at: dir, withIntermediateDirectories: true)
        defer { try? FileManager.default.removeItem(at: dir) }
        let old = dir.appendingPathComponent("UniversalMac_15.ipsw"), new = dir.appendingPathComponent("UniversalMac_26.IPSW")
        try Data().write(to: old); try Data().write(to: new)
        try Data().write(to: dir.appendingPathComponent("notes.txt"))
        try FileManager.default.setAttributes([.modificationDate: Date(timeIntervalSinceNow: -3600)], ofItemAtPath: old.path)
        XCTAssertEqual(VMInstaller.existingImages(in: [dir]).map(\.lastPathComponent), ["UniversalMac_26.IPSW", "UniversalMac_15.ipsw"])
    }
}

final class LeaseMarkerTests: XCTestCase {
    func testAMachineBelongsToOneLeaseAndAFreshCopyToNone() throws {
        let root = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString)
        defer { try? FileManager.default.removeItem(at: root) }
        let store = VMStore(root: root)
        try FileManager.default.createDirectory(at: store.base.directory, withIntermediateDirectories: true)
        try Data("disk".utf8).write(to: store.base.disk)
        try store.resetCurrent()
        XCTAssertNil(store.currentLease, "a fresh copy of the base belongs to nobody")
        try store.assignCurrentLease("lease-1")
        XCTAssertEqual(store.currentLease, "lease-1")
        // The renter's disk stays while the lease does…
        try Data("renter's files".utf8).write(to: store.current.disk)
        XCTAssertEqual(try String(contentsOf: store.current.disk, encoding: .utf8), "renter's files")
        // …and a reset for the next renter wipes both.
        try store.resetCurrent()
        XCTAssertNil(store.currentLease)
        XCTAssertEqual(try String(contentsOf: store.current.disk, encoding: .utf8), "disk")
    }
}
