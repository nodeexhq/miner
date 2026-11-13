import Foundation
import Network
import Virtualization

/// Where the virtual machines live on disk.
///
///     <root>/base/        the prepared machine every lease starts from
///     <root>/current/     the machine now running, a copy of base
///     <root>/restore.ipsw the macOS image base was installed from
///
/// A machine is a directory with its disk, its auxiliary storage and the
/// identifiers macOS expects to find unchanged from boot to boot.
public struct VMStore: Sendable {
    public let root: URL

    public init(root: URL) {
        self.root = root
    }

    public static var standard: VMStore {
        let support = FileManager.default.urls(for: .applicationSupportDirectory, in: .userDomainMask)[0]
        return VMStore(root: support.appendingPathComponent("NodeEx/Machines", isDirectory: true))
    }

    public var base: VMBundle { VMBundle(directory: root.appendingPathComponent("base", isDirectory: true)) }
    public var current: VMBundle { VMBundle(directory: root.appendingPathComponent("current", isDirectory: true)) }
    public var restoreImage: URL { root.appendingPathComponent("restore.ipsw") }

    /// Free space on the machines' volume, in GB, counting the macOS image
    /// as free: it is deleted once the base machine is installed.
    public func freeGB() -> UInt64 {
        let fm = FileManager.default
        try? fm.createDirectory(at: root, withIntermediateDirectories: true)
        let values = try? root.resourceValues(forKeys: [.volumeAvailableCapacityForImportantUsageKey])
        var free = UInt64(max(0, values?.volumeAvailableCapacityForImportantUsage ?? 0))
        if let image = try? fm.attributesOfItem(atPath: restoreImage.path)[.size] as? UInt64 { free += image }
        return free >> 30
    }

    /// Replaces the current machine with a fresh copy of the base. On APFS
    /// the copy shares the base's blocks, so it takes a moment and no extra
    /// space until the renter writes to it.
    public func resetCurrent() throws {
        let fm = FileManager.default
        guard base.exists else { throw VMError.noBaseImage }
        if fm.fileExists(atPath: current.directory.path) { try fm.removeItem(at: current.directory) }
        try fm.copyItem(at: base.directory, to: current.directory)
    }
}

public struct VMBundle: Sendable {
    public let directory: URL

    public var disk: URL { directory.appendingPathComponent("disk.img") }
    public var auxiliary: URL { directory.appendingPathComponent("aux.img") }
    public var hardwareModel: URL { directory.appendingPathComponent("hardware-model") }
    public var machineIdentifier: URL { directory.appendingPathComponent("machine-identifier") }
    public var macAddress: URL { directory.appendingPathComponent("mac-address") }

    public var exists: Bool { FileManager.default.fileExists(atPath: disk.path) }
}

public enum VMError: Error, CustomStringConvertible {
    case noBaseImage
    case unsupportedHost(String)
    case damagedBundle(String)
    case noAddress
    case notEnoughDisk(freeGB: UInt64)

    public var description: String {
        switch self {
        case .noBaseImage: return "there is no base machine yet; prepare one first"
        case .unsupportedHost(let why): return "this Mac cannot run the virtual machine: \(why)"
        case .damagedBundle(let what): return "the machine on disk is incomplete: \(what)"
        case .noAddress: return "the virtual machine did not get a network address"
        case .notEnoughDisk(let free): return "there is not enough free disk space for a machine: \(free) GB free, and \(VMResources.baseGB + VMResources.hostReserveGB + VMResources.minimumDiskGB) GB are needed"
        }
    }
}

public struct VMResources: Sendable {
    public var cpuCount: Int
    public var memoryBytes: UInt64
    public var diskBytes: UInt64

    public init(cpuCount: Int, memoryBytes: UInt64, diskBytes: UInt64) {
        self.cpuCount = cpuCount
        self.memoryBytes = memoryBytes
        self.diskBytes = diskBytes
    }

    /// What installing a machine costs on disk besides the machine's own
    /// disk: the installed base, and room the host keeps for itself (system
    /// updates, logs, the owner's own use).
    static let baseGB: UInt64 = 25
    static let hostReserveGB: UInt64 = 30
    /// Below this the guest's macOS leaves a renter too little to work in.
    static let minimumDiskGB: UInt64 = 80
    static let maximumDiskGB: UInt64 = 500

    /// The size of disk a new machine can be given on a volume with this
    /// much free space, in GB, or nil if there is not enough.
    ///
    /// A lease's machine starts as a copy that shares the base's blocks, but
    /// a renter may overwrite all of it: in the worst case the copy takes
    /// its full size on top of the base. The size is therefore what is left
    /// after the base and the host's reserve, rounded down to 10 GB, so a
    /// renter who fills their disk cannot fill the owner's.
    public static func diskGB(forFreeGB free: UInt64) -> UInt64? {
        guard free > baseGB + hostReserveGB else { return nil }
        let size = min((free - baseGB - hostReserveGB) / 10 * 10, maximumDiskGB)
        return size >= minimumDiskGB ? size : nil
    }

    /// What a rented machine gets: all but two of the host's cores and all
    /// but 4 GB of its memory, within what the framework allows. diskGB is
    /// used only when a machine is installed; a machine that exists keeps
    /// the disk it was made with.
    public static func forThisHost(diskGB: UInt64 = 128) -> VMResources {
        let cores = ProcessInfo.processInfo.processorCount
        let memory = ProcessInfo.processInfo.physicalMemory
        let gb: UInt64 = 1 << 30
        let cpu = max(VZVirtualMachineConfiguration.minimumAllowedCPUCount, min(VZVirtualMachineConfiguration.maximumAllowedCPUCount, cores - 2))
        let mem = max(VZVirtualMachineConfiguration.minimumAllowedMemorySize, min(VZVirtualMachineConfiguration.maximumAllowedMemorySize, memory > 8 * gb ? memory - 4 * gb : memory / 2))
        var resources = VMResources(cpuCount: cpu, memoryBytes: mem, diskBytes: diskGB * gb)
        // On a Mac that is also somebody's workstation, give the machine
        // less than everything: NODEEX_VM_CPUS and NODEEX_VM_MEMORY_GB.
        let env = ProcessInfo.processInfo.environment
        if let cpus = env["NODEEX_VM_CPUS"].flatMap(Int.init), cpus >= 1 { resources.cpuCount = min(cpus, cpu) }
        if let limit = env["NODEEX_VM_MEMORY_GB"].flatMap(UInt64.init), limit >= 4 { resources.memoryBytes = min(limit * gb, mem) }
        return resources
    }
}

/// Builds the framework's configuration for a machine on disk.
@MainActor
public enum VMConfiguration {
    public static func make(bundle: VMBundle, resources: VMResources, display: Bool = true, network guestNetwork: GuestNetwork? = nil) throws -> VZVirtualMachineConfiguration {
        guard let modelData = try? Data(contentsOf: bundle.hardwareModel), let model = VZMacHardwareModel(dataRepresentation: modelData) else {
            throw VMError.damagedBundle("hardware model")
        }
        guard model.isSupported else { throw VMError.unsupportedHost("the machine was made for a different kind of Mac") }
        guard let idData = try? Data(contentsOf: bundle.machineIdentifier), let identifier = VZMacMachineIdentifier(dataRepresentation: idData) else {
            throw VMError.damagedBundle("machine identifier")
        }

        let platform = VZMacPlatformConfiguration()
        platform.hardwareModel = model
        platform.machineIdentifier = identifier
        platform.auxiliaryStorage = VZMacAuxiliaryStorage(contentsOf: bundle.auxiliary)

        let config = VZVirtualMachineConfiguration()
        config.platform = platform
        config.bootLoader = VZMacOSBootLoader()
        config.cpuCount = resources.cpuCount
        config.memorySize = resources.memoryBytes

        let attachment = try VZDiskImageStorageDeviceAttachment(url: bundle.disk, readOnly: false)
        config.storageDevices = [VZVirtioBlockDeviceConfiguration(attachment: attachment)]

        // With a guest network every frame passes through the helper and
        // its rules. Without one the machine gets the system's NAT, which
        // lets it reach the owner's local network: for preparing the base
        // machine only, never for a renter.
        let network = VZVirtioNetworkDeviceConfiguration()
        if let guestNetwork {
            network.attachment = VZFileHandleNetworkDeviceAttachment(fileHandle: guestNetwork.machineEnd)
        } else {
            network.attachment = VZNATNetworkDeviceAttachment()
        }
        if let text = try? String(contentsOf: bundle.macAddress, encoding: .utf8), let mac = VZMACAddress(string: text.trimmingCharacters(in: .whitespacesAndNewlines)) {
            network.macAddress = mac
        }
        config.networkDevices = [network]

        if display {
            let graphics = VZMacGraphicsDeviceConfiguration()
            graphics.displays = [VZMacGraphicsDisplayConfiguration(widthInPixels: 1920, heightInPixels: 1200, pixelsPerInch: 80)]
            config.graphicsDevices = [graphics]
            config.keyboards = [VZUSBKeyboardConfiguration()]
            config.pointingDevices = [VZUSBScreenCoordinatePointingDeviceConfiguration()]
        }
        config.entropyDevices = [VZVirtioEntropyDeviceConfiguration()]
        config.memoryBalloonDevices = [VZVirtioTraditionalMemoryBalloonDeviceConfiguration()]
        try config.validate()
        return config
    }
}

/// Creates the base machine: downloads the macOS image Apple offers for this
/// Mac and installs it onto a new disk. What cannot be done here is the
/// first-run setup inside the guest (an account, Remote Login, Screen
/// Sharing): that is done once by hand, in a window, before the base is used.
@MainActor
public enum VMInstaller {
    /// The image for this host, downloading it unless it is already there.
    public static func restoreImage(store: VMStore, progress: @escaping @Sendable (Double) -> Void) async throws -> VZMacOSRestoreImage {
        let fm = FileManager.default
        try fm.createDirectory(at: store.root, withIntermediateDirectories: true)
        if fm.fileExists(atPath: store.restoreImage.path) {
            return try await VZMacOSRestoreImage.image(from: store.restoreImage)
        }
        let latest = try await VZMacOSRestoreImage.latestSupported
        let delegate = DownloadProgress(progress)
        let (temporary, _) = try await URLSession.shared.download(from: latest.url, delegate: delegate)
        try fm.moveItem(at: temporary, to: store.restoreImage)
        return try await VZMacOSRestoreImage.image(from: store.restoreImage)
    }

    /// Lays out an empty machine for the image and installs macOS on it.
    /// The resources for a machine about to be installed in this store:
    /// this host's cores and memory, and a disk sized to the free space.
    public static func resources(for store: VMStore) throws -> VMResources {
        let free = store.freeGB()
        guard let disk = VMResources.diskGB(forFreeGB: free) else { throw VMError.notEnoughDisk(freeGB: free) }
        return .forThisHost(diskGB: disk)
    }

    public static func install(image: VZMacOSRestoreImage, into bundle: VMBundle, resources: VMResources, progress: @escaping @Sendable (Double) -> Void) async throws {
        guard let requirements = image.mostFeaturefulSupportedConfiguration else {
            throw VMError.unsupportedHost("this version of macOS cannot be virtualized here")
        }
        guard requirements.hardwareModel.isSupported else { throw VMError.unsupportedHost("hardware model not supported") }
        let fm = FileManager.default
        try fm.createDirectory(at: bundle.directory, withIntermediateDirectories: true)

        // A sparse file: it takes space only as the guest writes.
        fm.createFile(atPath: bundle.disk.path, contents: nil)
        let handle = try FileHandle(forWritingTo: bundle.disk)
        try handle.truncate(atOffset: resources.diskBytes)
        try handle.close()

        try requirements.hardwareModel.dataRepresentation.write(to: bundle.hardwareModel)
        try VZMacMachineIdentifier().dataRepresentation.write(to: bundle.machineIdentifier)
        try VZMACAddress.randomLocallyAdministered().string.write(to: bundle.macAddress, atomically: true, encoding: .utf8)
        _ = try VZMacAuxiliaryStorage(creatingStorageAt: bundle.auxiliary, hardwareModel: requirements.hardwareModel)

        var needed = resources
        needed.cpuCount = max(needed.cpuCount, requirements.minimumSupportedCPUCount)
        needed.memoryBytes = max(needed.memoryBytes, requirements.minimumSupportedMemorySize)
        let config = try VMConfiguration.make(bundle: bundle, resources: needed)
        let machine = VZVirtualMachine(configuration: config)
        let installer = VZMacOSInstaller(virtualMachine: machine, restoringFromImageAt: image.url)
        let observation = installer.progress.observe(\.fractionCompleted) { p, _ in progress(p.fractionCompleted) }
        defer { observation.invalidate() }
        try await installer.install()
    }
}

final class DownloadProgress: NSObject, URLSessionDownloadDelegate, @unchecked Sendable {
    private let report: @Sendable (Double) -> Void

    init(_ report: @escaping @Sendable (Double) -> Void) {
        self.report = report
    }

    func urlSession(_ session: URLSession, downloadTask: URLSessionDownloadTask, didWriteData bytesWritten: Int64, totalBytesWritten: Int64, totalBytesExpectedToWrite: Int64) {
        if totalBytesExpectedToWrite > 0 { report(Double(totalBytesWritten) / Double(totalBytesExpectedToWrite)) }
    }

    func urlSession(_ session: URLSession, downloadTask: URLSessionDownloadTask, didFinishDownloadingTo location: URL) {}
}

/// A running machine.
@MainActor
public final class VMRunner: NSObject, VZVirtualMachineDelegate {
    public let machine: VZVirtualMachine
    public let bundle: VMBundle
    /// Called when the guest stops, whether it shut down or failed.
    public var onStop: ((Error?) -> Void)?

    /// The machine's filtered network, if it has one.
    public let network: GuestNetwork?

    public init(bundle: VMBundle, resources: VMResources, display: Bool = true, network: GuestNetwork? = nil) throws {
        self.bundle = bundle
        self.network = network
        machine = VZVirtualMachine(configuration: try VMConfiguration.make(bundle: bundle, resources: resources, display: display, network: network))
        super.init()
        machine.delegate = self
    }

    public func start() async throws {
        try await machine.start()
    }

    /// Cuts the power. The disk is thrown away after a lease, so there is
    /// nothing to shut down cleanly for.
    public func stop() async {
        if machine.canStop { try? await machine.stop() }
        network?.stop()
    }

    /// Where the host reaches the machine's SSH: a loopback port when the
    /// machine is on a guest network, its own address otherwise.
    public struct Endpoint: Sendable, Equatable {
        public let host: String
        public let port: UInt16
    }

    /// Waits until the machine's SSH answers and returns where.
    public func sshEndpoint(timeout: TimeInterval = 180) async throws -> Endpoint {
        guard let network else { return Endpoint(host: try await address(timeout: timeout), port: 22) }
        let deadline = Date().addingTimeInterval(timeout)
        while Date() < deadline {
            if await GuestNetwork.sshGreets(port: network.sshPort) { return Endpoint(host: "127.0.0.1", port: network.sshPort) }
            try await Task.sleep(nanoseconds: 2_000_000_000)
        }
        throw VMError.noAddress
    }

    /// The guest's address on the host's NAT network, read from the DHCP
    /// leases macOS keeps, once the guest's SSH port answers there. That
    /// takes some seconds after the guest boots.
    public func address(timeout: TimeInterval = 180) async throws -> String {
        guard let text = try? String(contentsOf: bundle.macAddress, encoding: .utf8) else { throw VMError.damagedBundle("MAC address") }
        let wanted = Self.normalize(text)
        let deadline = Date().addingTimeInterval(timeout)
        while Date() < deadline {
            // A lease can be left over from the last machine with this
            // MAC address, so the address counts only once the guest
            // answers on it.
            if let found = Self.leasedAddress(for: wanted), await Self.answers(found, port: 22) { return found }
            try await Task.sleep(nanoseconds: 2_000_000_000)
        }
        throw VMError.noAddress
    }

    /// Whether something accepts a TCP connection at host:port within two
    /// seconds.
    nonisolated static func answers(_ host: String, port: UInt16) async -> Bool {
        await withCheckedContinuation { continuation in
            let connection = NWConnection(host: NWEndpoint.Host(host), port: NWEndpoint.Port(rawValue: port)!, using: .tcp)
            let once = Once()
            let finish: @Sendable (Bool) -> Void = { ok in
                guard once.first() else { return }
                connection.cancel()
                continuation.resume(returning: ok)
            }
            connection.stateUpdateHandler = { state in
                switch state {
                case .ready: finish(true)
                case .failed, .cancelled: finish(false)
                default: break
                }
            }
            connection.start(queue: .global())
            DispatchQueue.global().asyncAfter(deadline: .now() + 2) { finish(false) }
        }
    }

    /// /var/db/dhcpd_leases writes each octet without its leading zero
    /// ("1,a:b:c:d:e:f"), so both sides are compared in that form.
    nonisolated static func normalize(_ mac: String) -> String {
        mac.trimmingCharacters(in: .whitespacesAndNewlines).lowercased().split(separator: ":").map { part in
            let trimmed = part.drop { $0 == "0" }
            return trimmed.isEmpty ? "0" : String(trimmed)
        }.joined(separator: ":")
    }

    nonisolated static func leasedAddress(for mac: String, leases: String? = nil) -> String? {
        guard let text = leases ?? (try? String(contentsOfFile: "/var/db/dhcpd_leases", encoding: .utf8)) else { return nil }
        var address: String?
        var found: String?
        for raw in text.split(separator: "\n") {
            let line = raw.trimmingCharacters(in: .whitespaces)
            if line == "{" { address = nil }
            if line.hasPrefix("ip_address=") { address = String(line.dropFirst("ip_address=".count)) }
            if line.hasPrefix("hw_address="), let comma = line.firstIndex(of: ",") {
                // The newest lease for an address is listed first.
                if normalize(String(line[line.index(after: comma)...])) == mac, found == nil { found = address }
            }
        }
        return found
    }

    public nonisolated func guestDidStop(_ virtualMachine: VZVirtualMachine) {
        Task { @MainActor in self.onStop?(nil) }
    }

    public nonisolated func virtualMachine(_ virtualMachine: VZVirtualMachine, didStopWithError error: Error) {
        Task { @MainActor in self.onStop?(error) }
    }
}

/// Lets exactly one caller through.
final class Once: @unchecked Sendable {
    private var done = false
    private let lock = NSLock()

    func first() -> Bool {
        lock.lock()
        defer { lock.unlock() }
        if done { return false }
        done = true
        return true
    }
}
