import Foundation

/// Runs the Go core's one-shot commands (status, enroll, abuse, stop-lease)
/// for the app.
public struct CoreCLI: Sendable {
    public let binary: URL
    /// The core's state directory; nil for its default.
    public let stateDirectory: URL?

    public init(binary: URL, stateDirectory: URL? = nil) {
        self.binary = binary
        self.stateDirectory = stateDirectory
    }

    public struct Result: Sendable {
        public let status: Int32
        public let output: String
        public let errors: String
        public var succeeded: Bool { status == 0 }
        /// abuse and stop-lease: the lease is not the one the platform says
        /// this node serves; it has already ended.
        public var leaseNotCurrent: Bool { status == 3 }
        /// abuse and stop-lease: the platform refused, for good.
        public var refused: Bool { status == 4 }
        /// The last line the core wrote, which is its verdict.
        public var message: String {
            // The core lists unmet requirements on standard output, one per
            // line after a dash, and only then gives up: those lines say
            // why, where its last line says only that it did.
            let reasons = output.split(separator: "\n").map { $0.trimmingCharacters(in: .whitespaces) }.filter { $0.hasPrefix("- ") }.map { String($0.dropFirst(2)) }
            if !reasons.isEmpty {
                return "This Mac cannot be a node:\n" + reasons.map { "• " + $0 }.joined(separator: "\n")
            }
            let text = errors.isEmpty ? output : errors
            let last = text.split(separator: "\n").last.map(String.init) ?? ""
            // "nvto-node: ..." is how the core prefixes its errors.
            return last.hasPrefix("nvto-node: ") ? String(last.dropFirst("nvto-node: ".count)) : last
        }
    }

    public func run(_ arguments: [String]) -> Result {
        let p = Process()
        p.executableURL = binary
        var args = arguments
        if let dir = stateDirectory, let command = arguments.first, ["status", "enroll", "run", "abuse", "stop-lease"].contains(command) {
            args += ["-dir", dir.path]
        }
        p.arguments = args
        let out = Pipe(), err = Pipe()
        p.standardOutput = out
        p.standardError = err
        do { try p.run() } catch { return Result(status: -1, output: "", errors: error.localizedDescription) }
        let o = out.fileHandleForReading.readDataToEndOfFile()
        let e = err.fileHandleForReading.readDataToEndOfFile()
        p.waitUntilExit()
        return Result(status: p.terminationStatus, output: String(data: o, encoding: .utf8) ?? "", errors: String(data: e, encoding: .utf8) ?? "")
    }

    /// Whether this Mac has been enrolled as a node.
    public var isEnrolled: Bool { run(["status"]).succeeded }

    /// Tells the platform the machine of lease was cut off the network for
    /// abuse. The core keeps trying for up to half an hour until the
    /// platform answers.
    public func reportAbuse(lease: String, reason: String, detail: String) -> Result {
        run(["abuse", "-lease", lease, "-reason", reason, "-detail", detail])
    }

    /// The owner's emergency stop of lease. Asked once.
    public func stopLease(lease: String, reason: String) -> Result {
        run(["stop-lease", "-lease", lease, "-reason", reason])
    }

    /// Enrolls this Mac with a one-time code from the hosting dashboard.
    public func enroll(code: String, api: String, force: Bool = false) -> Result {
        run(["enroll", "-api", api, "-token", code.trimmingCharacters(in: .whitespacesAndNewlines)] + (force ? ["-force"] : []))
    }
}
