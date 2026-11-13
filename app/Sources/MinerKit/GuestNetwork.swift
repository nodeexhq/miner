import Foundation
import Network

/// The network of one virtual machine, run by the nodeex-vmnet helper.
///
/// The machine's network device is one end of a datagram socket pair; the
/// helper holds the other end and is the only way in or out. That is what
/// makes the outbound rules binding: they are applied on the host, outside
/// the machine, where its administrator cannot reach them.
public final class GuestNetwork: @unchecked Sendable {
    /// The end the virtual machine's network device is attached to.
    public let machineEnd: FileHandle
    /// Loopback ports on the host that lead to the machine.
    public let sshPort: UInt16
    public let vncPort: UInt16

    private let process = Process()

    public init(helper: URL, macAddress: String, sshPort: UInt16, vncPort: UInt16) throws {
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
        process.arguments = ["-mac", macAddress, "-ssh-listen", "127.0.0.1:\(sshPort)", "-vnc-listen", "127.0.0.1:\(vncPort)"]
        process.standardInput = helperEnd
        process.standardOutput = FileHandle.nullDevice
        try process.run()
        try? helperEnd.close()
    }

    public func stop() {
        if process.isRunning { process.terminate() }
        process.waitUntilExit()
    }

    deinit {
        if process.isRunning { process.terminate() }
    }

    /// The helper shipped with the app, or beside the running program.
    public static func helper() -> URL? {
        let fm = FileManager.default
        if let override = ProcessInfo.processInfo.environment["NODEEX_VMNET_BIN"], fm.isExecutableFile(atPath: override) { return URL(fileURLWithPath: override) }
        let candidates = [
            Bundle.main.bundleURL.appendingPathComponent("Contents/Helpers/nodeex-vmnet"),
            Bundle.main.bundleURL.appendingPathComponent("nodeex-vmnet"),
            URL(fileURLWithPath: CommandLine.arguments[0]).deletingLastPathComponent().appendingPathComponent("nodeex-vmnet"),
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
