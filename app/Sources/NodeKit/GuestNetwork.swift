import Foundation
import Network

/// The network of one virtual machine, run by the nvto-vmnet helper.
///
/// The machine's network device is one end of a datagram socket pair; the
/// helper holds the other end and is the only way in or out. That is what
/// makes the outbound rules binding: they are applied on the host, outside
/// the machine, where its administrator cannot reach them.
public final class GuestNetwork: @unchecked Sendable {
    /// The end the virtual machine's network device is attached to.
    public let machineEnd: FileHandle
    /// Loopback ports on the host that lead to the machine. vncPort is nil
    /// when the host serves the machine's remote desktop itself
    /// (HostDesktop) and nothing is forwarded to the guest's.
    public let sshPort: UInt16
    public let vncPort: UInt16?

    private let process = Process()

    /// guestDesktopPort, if given, opens that port on the machine's gateway
    /// to the host's remote desktop at 127.0.0.1:hostDesktopPort, for the
    /// guest itself (a desktop carried inside the renter's SSH). Only the
    /// machine is on that network; nothing on the host or the owner's
    /// network can reach it.
    public init(helper: URL, macAddress: String, sshPort: UInt16, vncPort: UInt16?, guestDesktopPort: UInt16? = nil,
                hostDesktopPort: UInt16 = MachineController.vncPort, limits: URL) throws {
        self.sshPort = sshPort
        self.vncPort = vncPort
        var fds: [Int32] = [0, 0]
        guard socketpair(AF_UNIX, SOCK_DGRAM, 0, &fds) == 0 else { throw VMError.unsupportedHost("could not create the machine's network socket") }
        // Room for bursts in both directions, as the framework's
        // documentation asks of a file-handle network device.
        for fd in fds {
            var send: Int32 = 1 << 20, receive: Int32 = 4 << 20
            setsockopt(fd, SOL_SOCKET, SO_SNDBUF, &send, socklen_t(MemoryLayout<Int32>.size))
            setsockopt(fd, SOL_SOCKET, SO_RCVBUF, &receive, socklen_t(MemoryLayout<Int32>.size))
        }
        machineEnd = FileHandle(fileDescriptor: fds[0], closeOnDealloc: true)
        let helperEnd = FileHandle(fileDescriptor: fds[1], closeOnDealloc: true)
        process.executableURL = helper
        process.arguments = ["-mac", macAddress, "-ssh-listen", "127.0.0.1:\(sshPort)", "-vnc-listen", vncPort.map { "127.0.0.1:\($0)" } ?? "", "-limits", limits.path]
        if let guestDesktopPort {
            process.arguments! += ["-guest-desktop-port", String(guestDesktopPort), "-guest-desktop-target", "127.0.0.1:\(hostDesktopPort)"]
        }
        // The helper asks the platform for this Mac's public address, to
        // refuse it to the machine; the same platform the app talks to.
        if let api = ProcessInfo.processInfo.environment["NVTO_API"], !api.isEmpty {
            process.arguments! += ["-api", api]
        }
        process.standardInput = helperEnd
        // Standard output carries JSON lines: {"ready":true,...} once, and
        // {"event":"abuse",...} if the helper cut the machine's network for
        // scanning or flooding from the owner's address.
        let output = Pipe()
        process.standardOutput = output
        let lines = LineBuffer()
        output.fileHandleForReading.readabilityHandler = { [weak self] handle in
            let data = handle.availableData
            if data.isEmpty { handle.readabilityHandler = nil; return }
            for line in lines.append(data) { self?.received(line) }
        }
        // What the helper says goes to the system log, where an owner (or
        // support) can find why a machine has no network.
        let errors = Pipe()
        process.standardError = errors
        errors.fileHandleForReading.readabilityHandler = { handle in
            let data = handle.availableData
            if data.isEmpty { handle.readabilityHandler = nil; return }
            for line in String(decoding: data, as: UTF8.self).split(separator: "\n") {
                NSLog("NVTO network: %@", String(line))
            }
        }
        process.terminationHandler = { [weak self] p in
            NSLog("NVTO network: helper exited with status %d", p.terminationStatus)
            self?.onExit?()
        }
        try process.run()
        try? helperEnd.close()
    }

    /// Called if the helper stops on its own: the machine has lost its
    /// network and should be replaced.
    public var onExit: (@Sendable () -> Void)?

    /// The helper cut the machine's network, both ways, for abuse: what it
    /// saw the machine do. The cut lasts as long as the helper; release()
    /// does not lift it.
    public struct Abuse: Sendable, Equatable {
        /// fan_out, port_scan, half_open, sustained_new_flows or sustained_udp.
        public let reason: String
        public let detail: String
        /// When the helper saw it, RFC 3339.
        public let time: String
    }

    /// Called, on a background queue, when the helper reports abuse.
    public var onAbuse: (@Sendable (Abuse) -> Void)?

    private func received(_ line: String) {
        guard let object = try? JSONSerialization.jsonObject(with: Data(line.utf8)) as? [String: Any],
              object["event"] as? String == "abuse" else { return }
        let abuse = Abuse(reason: object["reason"] as? String ?? "", detail: object["detail"] as? String ?? "", time: object["time"] as? String ?? "")
        NSLog("NVTO network: cut for abuse: %@: %@", abuse.reason, abuse.detail)
        onAbuse?(abuse)
    }

    /// Drops all of the machine's traffic, both ways, until release: the
    /// helper holds the network while the lease is not confirmed.
    public func hold() { signal(SIGUSR1) }
    public func release() { signal(SIGUSR2) }

    private func signal(_ number: Int32) {
        if process.isRunning { kill(process.processIdentifier, number) }
    }

    public func stop() {
        onExit = nil
        if process.isRunning { process.terminate() }
        process.waitUntilExit()
    }

    deinit {
        if process.isRunning { process.terminate() }
    }

    /// The helper shipped with the app, or beside the running program.
    public static func helper() -> URL? {
        let fm = FileManager.default
        if let override = ProcessInfo.processInfo.environment["NVTO_VMNET_BIN"], fm.isExecutableFile(atPath: override) { return URL(fileURLWithPath: override) }
        let candidates = [
            Bundle.main.bundleURL.appendingPathComponent("Contents/Helpers/nvto-vmnet"),
            Bundle.main.bundleURL.appendingPathComponent("nvto-vmnet"),
            URL(fileURLWithPath: CommandLine.arguments[0]).deletingLastPathComponent().appendingPathComponent("nvto-vmnet"),
        ]
        return candidates.first { fm.isExecutableFile(atPath: $0.path) }
    }

    /// Whether an SSH server greets a connection to a loopback port within
    /// three seconds. A forwarded port accepts connections even while the
    /// machine behind it is still starting, so accepting is not enough.
    static func sshGreets(port: UInt16) async -> Bool {
        await withCheckedContinuation { continuation in
            let connection = NWConnection(host: .ipv4(.loopback), port: NWEndpoint.Port(rawValue: port)!, using: .tcp)
            let once = Once()
            let finish: @Sendable (Bool) -> Void = { ok in
                guard once.first() else { return }
                connection.cancel()
                continuation.resume(returning: ok)
            }
            connection.stateUpdateHandler = { state in
                switch state {
                case .ready:
                    connection.receive(minimumIncompleteLength: 4, maximumLength: 64) { data, _, _, _ in
                        finish(data.map { $0.starts(with: Data("SSH-".utf8)) } ?? false)
                    }
                case .failed, .cancelled: finish(false)
                default: break
                }
            }
            connection.start(queue: .global())
            DispatchQueue.global().asyncAfter(deadline: .now() + 3) { finish(false) }
        }
    }
}
