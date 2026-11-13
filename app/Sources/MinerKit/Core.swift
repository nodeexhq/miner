import Foundation

/// A state change reported by `nodeex-miner run -events`, one JSON object
/// per line. Unknown fields and events are ignored, so the core can add to
/// them without breaking the app.
public struct CoreEvent: Decodable, Sendable {
    public let event: String
    public let state: String?
    public let node: String?
    public let addr: String?
    public let lease: String?
    public let endsAt: Date?
    public let sshKeys: [String]?

    enum CodingKeys: String, CodingKey {
        case event, state, node, addr, lease
        case endsAt = "ends_at"
        case sshKeys = "ssh_keys"
    }
}

/// Runs the Go core as a child process, restarts it if it dies, and hands
/// its events and log lines to the app.
public final class CoreSupervisor: @unchecked Sendable {
    public struct Options: Sendable {
        /// The `nodeex-miner` binary.
        public var binary: URL
        /// Its state directory; nil for the core's default.
        public var stateDirectory: URL?
        /// Where the core forwards renters' connections.
        public var sshTarget: String
        public var vncTarget: String
        /// The machine each lease is given, for the node to tell the
        /// platform; nil if leases are not given a machine of their own.
        public var machine: MachineSize?

        public init(binary: URL, stateDirectory: URL? = nil, sshTarget: String = "127.0.0.1:22", vncTarget: String = "127.0.0.1:5900", machine: MachineSize? = nil) {
            self.binary = binary
            self.stateDirectory = stateDirectory
            self.sshTarget = sshTarget
            self.vncTarget = vncTarget
            self.machine = machine
        }
    }

    public var onEvent: (@Sendable (CoreEvent) -> Void)?
    public var onLog: (@Sendable (String) -> Void)?
    /// Called with true when the core starts and false when it has exited.
    public var onRunning: (@Sendable (Bool) -> Void)?

    private let options: Options
    private let queue = DispatchQueue(label: "com.nodeex.miner.core")
    private var process: Process?
    private var wanted = false
    private var restartDelay: TimeInterval = 2

    public init(options: Options) {
        self.options = options
    }

    public func start() {
        queue.async {
            self.wanted = true
            self.launch()
        }
    }

    /// Stops the core and does not restart it. The core closes its
    /// connections on SIGTERM.
    public func stop() {
        queue.async {
            self.wanted = false
            self.process?.terminate()
        }
    }

    private func launch() {
        guard wanted, process == nil else { return }
        let p = Process()
        p.executableURL = options.binary
        var args = ["run", "-events", "-ssh-target", options.sshTarget, "-vnc-target", options.vncTarget]
        if let dir = options.stateDirectory { args += ["-dir", dir.path] }
        if let m = options.machine {
            args += ["-machine-cpus", String(m.cpuCount), "-machine-memory-gb", String(m.memoryGB), "-machine-disk-gb", String(m.diskGB)]
        }
        p.arguments = args
        let out = Pipe(), err = Pipe()
        p.standardOutput = out
        p.standardError = err
        read(out) { [weak self] line in self?.handleEvent(line) }
        read(err) { [weak self] line in self?.onLog?(line) }
        p.terminationHandler = { [weak self] finished in
            self?.queue.async { self?.exited(finished) }
        }
        do {
            try p.run()
            process = p
            onRunning?(true)
        } catch {
            onLog?("could not start the Miner core: \(error.localizedDescription)")
            scheduleRestart()
        }
    }

    private func exited(_ finished: Process) {
        process = nil
        onRunning?(false)
        onLog?("the Miner core exited with status \(finished.terminationStatus)")
        // A core that ran for a while starts again quickly; one that keeps
        // dying is retried less and less often.
        scheduleRestart()
    }

    private func scheduleRestart() {
        guard wanted else { return }
        let delay = restartDelay
        restartDelay = min(restartDelay * 2, 120)
        queue.asyncAfter(deadline: .now() + delay) { self.launch() }
        queue.asyncAfter(deadline: .now() + 300) { if self.process != nil { self.restartDelay = 2 } }
    }

    private func handleEvent(_ line: String) {
        let decoder = JSONDecoder()
        decoder.dateDecodingStrategy = .iso8601
        // Go writes fractional seconds, which .iso8601 refuses.
        decoder.dateDecodingStrategy = .custom { d in
            let s = try d.singleValueContainer().decode(String.self)
            let f = ISO8601DateFormatter()
            f.formatOptions = [.withInternetDateTime, .withFractionalSeconds]
            if let date = f.date(from: s) { return date }
            f.formatOptions = [.withInternetDateTime]
            if let date = f.date(from: s) { return date }
            throw DecodingError.dataCorrupted(.init(codingPath: d.codingPath, debugDescription: "not a date: \(s)"))
        }
        guard let data = line.data(using: .utf8), let event = try? decoder.decode(CoreEvent.self, from: data) else { return }
        onEvent?(event)
    }

    /// Delivers a pipe's output a line at a time.
    private func read(_ pipe: Pipe, _ deliver: @escaping @Sendable (String) -> Void) {
        let buffer = LineBuffer()
        pipe.fileHandleForReading.readabilityHandler = { handle in
            let data = handle.availableData
            if data.isEmpty {
                handle.readabilityHandler = nil
                return
            }
            for line in buffer.append(data) { deliver(line) }
        }
    }
}

final class LineBuffer: @unchecked Sendable {
    private var pending = Data()
    private let lock = NSLock()

    func append(_ data: Data) -> [String] {
        lock.lock()
        defer { lock.unlock() }
        pending.append(data)
        var lines: [String] = []
        while let newline = pending.firstIndex(of: 0x0A) {
            let line = pending.subdata(in: pending.startIndex..<newline)
            pending.removeSubrange(pending.startIndex...newline)
            if let text = String(data: line, encoding: .utf8), !text.isEmpty { lines.append(text) }
        }
        return lines
    }
}

/// How large the machine a renter gets is.
public struct MachineSize: Sendable, Equatable {
    public let cpuCount: Int
    public let memoryGB: Int
    public let diskGB: Int

    public init(cpuCount: Int, memoryGB: Int, diskGB: Int) {
        self.cpuCount = cpuCount
        self.memoryGB = memoryGB
        self.diskGB = diskGB
    }
}
