import Foundation
import Network

/// Forwards a TCP port on this Mac's loopback address to wherever the
/// current virtual machine is. The Go core is told to send renters'
/// connections to the loopback port once, at start; the app points the
/// forwarder at each new machine, and at nothing between machines.
public final class Forwarder: @unchecked Sendable {
    private let listener: NWListener
    private let queue = DispatchQueue(label: "com.nodeex.miner.forwarder")
    private var target: NWEndpoint?
    private var live: [ObjectIdentifier: (NWConnection, NWConnection)] = [:]

    public init(port: UInt16) throws {
        let parameters = NWParameters.tcp
        parameters.requiredLocalEndpoint = .hostPort(host: .ipv4(.loopback), port: NWEndpoint.Port(rawValue: port)!)
        parameters.allowLocalEndpointReuse = true
        listener = try NWListener(using: parameters)
        listener.newConnectionHandler = { [weak self] connection in self?.accept(connection) }
        listener.start(queue: queue)
    }

    /// Points the port at host:port, or at nothing. Connections to the
    /// previous target are closed: they belonged to the previous machine.
    public func setTarget(host: String?, port: UInt16) {
        queue.async {
            self.target = host.map { .hostPort(host: NWEndpoint.Host($0), port: NWEndpoint.Port(rawValue: port)!) }
            for (a, b) in self.live.values {
                a.cancel()
                b.cancel()
            }
            self.live.removeAll()
        }
    }

    public func stop() {
        listener.cancel()
        setTarget(host: nil, port: 0)
    }

    private func accept(_ down: NWConnection) {
        guard let target else {
            down.cancel()
            return
        }
        let up = NWConnection(to: target, using: .tcp)
        let key = ObjectIdentifier(down)
        live[key] = (down, up)
        let close: @Sendable () -> Void = { [weak self] in
            self?.queue.async {
                guard let pair = self?.live.removeValue(forKey: key) else { return }
                pair.0.cancel()
                pair.1.cancel()
            }
        }
        down.start(queue: queue)
        up.start(queue: queue)
        pipe(from: down, to: up, close: close)
        pipe(from: up, to: down, close: close)
    }

    private func pipe(from: NWConnection, to: NWConnection, close: @escaping @Sendable () -> Void) {
        from.receive(minimumIncompleteLength: 1, maximumLength: 64 * 1024) { [weak self] data, _, finished, error in
            if let data, !data.isEmpty {
                to.send(content: data, completion: .contentProcessed { sendError in
                    if sendError != nil || finished || error != nil { close() } else { self?.pipe(from: from, to: to, close: close) }
                })
            } else {
                close()
            }
        }
    }
}
