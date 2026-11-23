import XCTest
@testable import NodeKit

@MainActor
final class AbuseReporterTests: XCTestCase {
    private func store() -> VMStore {
        let root = FileManager.default.temporaryDirectory.appendingPathComponent("abuse-\(UUID().uuidString)")
        addTeardownBlock { try? FileManager.default.removeItem(at: root) }
        return VMStore(root: root)
    }

    private func settle(_ until: @escaping () -> Bool) async {
        for _ in 0..<500 where !until() { try? await Task.sleep(nanoseconds: 2_000_000) }
    }

    private actor Script {
        var outcomes: [AbuseOutcome]
        var sent: [AbuseRecord] = []
        init(_ outcomes: [AbuseOutcome]) { self.outcomes = outcomes }
        func next(_ r: AbuseRecord) -> AbuseOutcome {
            sent.append(r)
            return outcomes.isEmpty ? .reported : outcomes.removeFirst()
        }
    }

    func testRetriesUntilThePlatformAnswers() async throws {
        let store = store()
        try AbuseRecord(lease: "lease-1", reason: "half_open", detail: "d", time: "t").save(store)
        let script = Script([.failed("offline"), .failed("offline"), .reported])
        let reporter = AbuseReporter(store: store, retryAfter: 0.01) { await script.next($0) }
        var outcomes: [AbuseOutcome] = []
        reporter.onOutcome = { _, o in outcomes.append(o) }
        reporter.reportPending()
        reporter.reportPending() // already being sent
        await settle { outcomes.count == 3 }
        XCTAssertEqual(outcomes, [.failed("offline"), .failed("offline"), .reported])
        XCTAssertEqual(AbuseRecord.load(store)?.reported, true)
        let sent = await script.sent
        XCTAssertEqual(sent.map(\.lease), ["lease-1", "lease-1", "lease-1"])
        // Reported: not sent again, after a relaunch either.
        AbuseReporter(store: store, retryAfter: 0.01) { await script.next($0) }.reportPending()
        try await Task.sleep(nanoseconds: 50_000_000)
        let count = await script.sent.count
        XCTAssertEqual(count, 3)
    }

    func testAnEndedLeaseOrARefusalSettlesTheReport() async throws {
        for outcome in [AbuseOutcome.leaseEnded, .refused("invalid_reason")] {
            let store = store()
            try AbuseRecord(lease: "lease-1", reason: "fan_out", detail: "", time: "").save(store)
            let script = Script([outcome])
            let reporter = AbuseReporter(store: store, retryAfter: 0.01) { await script.next($0) }
            var seen: [AbuseOutcome] = []
            reporter.onOutcome = { _, o in seen.append(o) }
            reporter.reportPending()
            await settle { !seen.isEmpty }
            try await Task.sleep(nanoseconds: 50_000_000)
            XCTAssertEqual(seen, [outcome])
            XCTAssertEqual(AbuseRecord.load(store)?.reported, true)
        }
    }

    func testNothingToReport() async {
        let reporter = AbuseReporter(store: store()) { _ in XCTFail("sent"); return .reported }
        reporter.reportPending()
        try? await Task.sleep(nanoseconds: 20_000_000)
    }

    func testOutcomesFromTheCore() {
        func result(_ status: Int32, _ out: String = "", _ err: String = "") -> CoreCLI.Result {
            CoreCLI.Result(status: status, output: out, errors: err)
        }
        XCTAssertEqual(AbuseOutcome(result(0, "{}\n")), .reported)
        XCTAssertEqual(AbuseOutcome(result(3, "", "nvto-node: POST /v1/nodes/n/abuse: no (lease_not_current)\n")), .leaseEnded)
        XCTAssertEqual(AbuseOutcome(result(4, "", "nvto-node: refused\n")), .refused("refused"))
        XCTAssertEqual(AbuseOutcome(result(1, "", "nvto-node: offline\n")), .failed("offline"))

        let json = #"{"lease_id":"lease-1","ended_at":"2026-11-21T10:00:00.123456Z","refunded":"12.50","hosting":false,"paused_until":"2026-11-28T10:00:00Z"}"#
        guard case .stopped(let reply) = StopLeaseOutcome(result(0, json + "\n")) else { return XCTFail("not stopped") }
        XCTAssertEqual(reply.leaseID, "lease-1")
        XCTAssertEqual(reply.refunded, "12.50")
        XCTAssertFalse(reply.hosting)
        XCTAssertEqual(reply.pausedUntil, ISO8601DateFormatter().date(from: "2026-11-28T10:00:00Z"))
        guard case .stopped(let unpaused) = StopLeaseOutcome(result(0, #"{"lease_id":"l","ended_at":"2026-11-21T10:00:00Z","refunded":"1.00","hosting":false}"#)) else { return XCTFail() }
        XCTAssertNil(unpaused.pausedUntil)
        XCTAssertEqual(StopLeaseOutcome(result(3, "", "nvto-node: x (lease_not_current)")), .leaseEnded)
        XCTAssertEqual(StopLeaseOutcome(result(1, "", "nvto-node: offline")), .failed("offline"))
        if case .failed = StopLeaseOutcome(result(0, "garbage")) {} else { XCTFail("garbage reply") }
    }
}
