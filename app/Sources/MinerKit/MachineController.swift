import Foundation

/// Keeps one fresh virtual machine ready for a renter and replaces it after
/// every lease: the machine a renter used is destroyed, never handed on.
@MainActor
public final class MachineController {
    public enum State: Equatable, Sendable {
        /// No base machine has been prepared on this Mac.
        case noBase
        case starting
        /// The machine answers; the host reaches it at this loopback port.
        case ready(sshPort: UInt16)
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
    private let helper: URL
    private var runner: VMRunner?
    private var generation = 0
    private var wantedKeys: [String] = []

    public init(store: VMStore = .standard, resources: VMResources = .forThisHost()) throws {
        self.store = store
        self.resources = resources
        // A renter's machine is never started on the unfiltered network.
        guard let helper = GuestNetwork.helper() else { throw VMError.unsupportedHost("the nodeex-vmnet helper is missing") }
        self.helper = helper
    }

    /// Throws away whatever machine exists and starts a new one from the
    /// base. Called at launch and whenever a lease ends.
    public func renew() async {
        generation += 1
        let mine = generation
        // The next machine is for the next renter: the last one's keys go.
        wantedKeys = []
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
            guard let mac = try? String(contentsOf: store.current.macAddress, encoding: .utf8).trimmingCharacters(in: .whitespacesAndNewlines) else {
                throw VMError.damagedBundle("MAC address")
            }
            // The helper forwards the two loopback ports for as long as this
            // machine lives, and applies the outbound rules to it.
            let network = try GuestNetwork(helper: helper, macAddress: mac, sshPort: Self.sshPort, vncPort: Self.vncPort)
            let next = try VMRunner(bundle: store.current, resources: resources, network: network)
            next.onStop = { [weak self] error in
                guard let self, self.generation == mine else { return }
                // The renter shut the machine down, or it crashed: either
                // way they get a working machine back, with a clean disk.
                self.state = .failed(error.map { "\($0.localizedDescription)" } ?? "the machine was shut down")
                Task { await self.renew() }
            }
            runner = next
            try await next.start()
            let endpoint = try await next.sshEndpoint()
            guard generation == mine else { return }
            state = .ready(sshPort: endpoint.port)
            applyKeys()
        } catch {
            guard generation == mine else { return }
            state = .failed("\(error)")
        }
    }

    /// Installs the renter's SSH keys in the machine that is running, in
    /// place of any that were there. With no keys only the host can log in.
    /// The keys are remembered and applied to a machine still starting.
    public func authorize(renterKeys: [String]) {
        wantedKeys = renterKeys
        applyKeys()
    }

    private func applyKeys() {
        guard case .ready(let port) = state, let account = Provisioning.account(store: store) else { return }
        let endpoint = VMRunner.Endpoint(host: "127.0.0.1", port: port)
        let keys = wantedKeys, store = store
        Task.detached {
            do {
                try Provisioning.authorize(renterKeys: keys, at: endpoint, account: account, store: store)
            } catch {
                NSLog("NodeEx: could not install the renter's keys: \(error)")
            }
        }
    }

    public func shutDown() async {
        generation += 1
        await runner?.stop()
        runner = nil
    }
}
