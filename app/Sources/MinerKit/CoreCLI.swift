import Foundation

/// Runs the Go core's one-shot commands (status, enroll) for the app.
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
        /// The last line the core wrote, which is its verdict.
        public var message: String {
            let text = errors.isEmpty ? output : errors
            return text.split(separator: "\n").last.map(String.init) ?? ""
        }
    }

    public func run(_ arguments: [String]) -> Result {
        let p = Process()
        p.executableURL = binary
        var args = arguments
        if let dir = stateDirectory, let command = arguments.first, ["status", "enroll", "run"].contains(command) {
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

    /// Enrolls this Mac with a one-time code from the mining dashboard.
    public func enroll(code: String, api: String, force: Bool = false) -> Result {
        run(["enroll", "-api", api, "-token", code.trimmingCharacters(in: .whitespacesAndNewlines)] + (force ? ["-force"] : []))
    }
}
