import Foundation

/// Keeps one fresh virtual machine ready for a renter and replaces it after
/// every lease: the machine a renter used is destroyed, never handed on.
@MainActor
public final class MachineController {
    public enum State: Equatable, Sendable {
        /// No base machine has been prepared on this Mac.
        case noBase
        case starting
        case ready(address: String)
        case failed(String)
    }

    /// Loopback ports the Go core forwards renters to; the controller points
    /// them at whichever machine is current.
    public static let sshPort: UInt16 = 22022
    public static let vncPort: UInt16 = 25900

    public private(set) var state: State = .noBase {
        didSet { if state != oldValue { onChange?(state) } }
    }
    public var onChange: ((State) -> Void)?

    private let store: VMStore
    private let resources: VMResources
    private let ssh: Forwarder
    private let vnc: Forwarder
    private var runner: VMRunner?
    private var generation = 0

    public init(store: VMStore = .standard, resources: VMResources = .forThisHost()) throws {
        self.store = store
        self.resources = resources
        ssh = try Forwarder(port: Self.sshPort)
        vnc = try Forwarder(port: Self.vncPort)
    }

    /// Throws away whatever machine exists and starts a new one from the
    /// base. Called at launch and whenever a lease ends.
    public func renew() async {
        generation += 1
        let mine = generation
        ssh.setTarget(host: nil, port: 0)
        vnc.setTarget(host: nil, port: 0)
        if let old = runner {
            runner = nil
            old.onStop = nil
            await old.stop()
        }
        guard store.base.exists else {
            state = .noBase
            return
        }
        state = .starting
        do {
            try store.resetCurrent()
            let next = try VMRunner(bundle: store.current, resources: resources)
            next.onStop = { [weak self] error in
                guard let self, self.generation == mine else { return }
                // The renter shut the machine down, or it crashed: either
                // way they get a working machine back, with a clean disk.
                self.state = .failed(error.map { "\($0.localizedDescription)" } ?? "the machine was shut down")
                Task { await self.renew() }
            }
            runner = next
            try await next.start()
            let address = try await next.address()
            guard generation == mine else { return }
            ssh.setTarget(host: address, port: 22)
            vnc.setTarget(host: address, port: 5900)
            state = .ready(address: address)
        } catch {
            guard generation == mine else { return }
            state = .failed("\(error)")
        }
    }

    public func shutDown() async {
        generation += 1
        ssh.stop()
        vnc.stop()
        await runner?.stop()
        runner = nil
    }
}
