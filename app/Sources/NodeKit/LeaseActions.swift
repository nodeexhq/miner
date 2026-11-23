import Foundation

// The two ways a node ends its own lease at once: an abuse report, when the
// machine's network helper cut it off for scanning or flooding from the
// owner's address, and the owner's emergency stop. Both go to the platform
// through the Go core, which holds the node's device key (CoreCLI).

/// A machine cut off the network for abuse, for the lease it belonged to.
/// Kept on disk beside the machines (abuse.json) until the machine is
/// discarded, so that neither a restart of the machine nor of the app gives
/// that lease a machine with its network back, and so that a report not
/// yet delivered is delivered after a relaunch.
public struct AbuseRecord: Codable, Equatable, Sendable {
    public let lease: String
    public let reason: String
    public let detail: String
    /// When the helper saw it, RFC 3339.
    public let time: String
    /// The platform has answered the report (accepted it, or said the lease
    /// had already ended).
    public var reported: Bool

    public init(lease: String, reason: String, detail: String, time: String, reported: Bool = false) {
        self.lease = lease
        self.reason = reason
        self.detail = detail
        self.time = time
        self.reported = reported
    }

    static func file(_ store: VMStore) -> URL { store.root.appendingPathComponent("abuse.json") }

    public static func load(_ store: VMStore) -> AbuseRecord? {
        guard let data = try? Data(contentsOf: file(store)) else { return nil }
        return try? JSONDecoder().decode(AbuseRecord.self, from: data)
    }

    public func save(_ store: VMStore) throws {
        try FileManager.default.createDirectory(at: store.root, withIntermediateDirectories: true)
        try JSONEncoder().encode(self).write(to: Self.file(store), options: .atomic)
    }

    static func remove(_ store: VMStore) {
        try? FileManager.default.removeItem(at: file(store))
    }
}

/// What came of an abuse report.
public enum AbuseOutcome: Equatable, Sendable {
    /// The platform ended the lease.
    case reported
    /// The lease had already ended.
    case leaseEnded
    /// The platform refused the report; trying again would not change that.
    case refused(String)
    /// The platform could not be reached; try again later.
    case failed(String)

    public init(_ result: CoreCLI.Result) {
        if result.succeeded { self = .reported }
        else if result.leaseNotCurrent { self = .leaseEnded }
        else if result.refused { self = .refused(result.message) }
        else { self = .failed(result.message) }
    }

    /// Whether the report is settled, one way or the other.
    var settled: Bool {
        if case .failed = self { return false }
        return true
    }
}

/// Delivers abuse reports: as soon as the machine is cut off, again after a
/// relaunch if it was not delivered, and again every retryAfter while the
/// platform cannot be reached (the core itself keeps trying for half an
/// hour each time).
@MainActor
public final class AbuseReporter {
    public typealias Send = @Sendable (AbuseRecord) async -> AbuseOutcome
    private let store: VMStore
    private let send: Send
    private let retryAfter: TimeInterval
    private var sending: String?
    /// Called with each answer, for the menu.
    public var onOutcome: ((AbuseRecord, AbuseOutcome) -> Void)?

    public init(store: VMStore, retryAfter: TimeInterval = 300, send: @escaping Send) {
        self.store = store
        self.retryAfter = retryAfter
        self.send = send
    }

    /// Reports the recorded abuse unless it is reported, or being reported.
    public func reportPending() {
        guard let record = AbuseRecord.load(store), !record.reported, sending != record.lease else { return }
        sending = record.lease
        Task {
            let outcome = await send(record)
            sending = nil
            onOutcome?(record, outcome)
            guard var current = AbuseRecord.load(store), current.lease == record.lease else { return }
            if outcome.settled {
                current.reported = true
                try? current.save(store)
                NSLog("NVTO: abuse in lease %@ reported: %@", record.lease, "\(outcome)")
                return
            }
            NSLog("NVTO: abuse in lease %@ not reported yet: %@", record.lease, "\(outcome)")
            try? await Task.sleep(nanoseconds: UInt64(retryAfter * 1_000_000_000))
            reportPending()
        }
    }
}

/// The platform's answer to an emergency stop.
public struct StopLeaseReply: Decodable, Equatable, Sendable {
    public let leaseID: String
    /// What the renter got back: the period's whole rent, as the platform
    /// formats money.
    public let refunded: String
    /// False: the stop switched hosting off on this node.
    public let hosting: Bool
    /// Set when the node was stopped too often and takes no orders until then.
    public let pausedUntil: Date?

    enum CodingKeys: String, CodingKey {
        case leaseID = "lease_id", refunded, hosting
        case pausedUntil = "paused_until"
    }
}

public enum StopLeaseOutcome: Equatable, Sendable {
    case stopped(StopLeaseReply)
    case leaseEnded
    case failed(String)

    public init(_ result: CoreCLI.Result) {
        if result.leaseNotCurrent {
            self = .leaseEnded
            return
        }
        guard result.succeeded else {
            self = .failed(result.message.isEmpty ? "the platform could not be reached" : result.message)
            return
        }
        let line = result.output.split(separator: "\n").last.map(String.init) ?? ""
        let decoder = JSONDecoder()
        decoder.dateDecodingStrategy = .custom { d in
            let s = try d.singleValueContainer().decode(String.self)
            let f = ISO8601DateFormatter()
            f.formatOptions = [.withInternetDateTime, .withFractionalSeconds]
            if let date = f.date(from: s) { return date }
            f.formatOptions = [.withInternetDateTime]
            if let date = f.date(from: s) { return date }
            throw DecodingError.dataCorrupted(.init(codingPath: d.codingPath, debugDescription: "not a date: \(s)"))
        }
        if let reply = try? decoder.decode(StopLeaseReply.self, from: Data(line.utf8)) {
            self = .stopped(reply)
        } else {
            // Stopped, but the reply could not be read: say so plainly.
            self = .failed("the lease was stopped, but the platform's reply could not be read")
        }
    }
}
