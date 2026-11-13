import Foundation
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

    public var description: String {
        switch self {
        case .noBaseImage: return "there is no base machine yet; prepare one first"
        case .unsupportedHost(let why): return "this Mac cannot run the virtual machine: \(why)"
        case .damagedBundle(let what): return "the machine on disk is incomplete: \(what)"
        case .noAddress: return "the virtual machine did not get a network address"
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

    /// What a rented machine gets: all but two of the host's cores and all
    /// but 4 GB of its memory, within what the framework allows.
    public static func forThisHost(diskGB: UInt64 = 128) -> VMResources {
        let cores = ProcessInfo.processInfo.processorCount
        let memory = ProcessInfo.processInfo.physicalMemory
        let gb: UInt64 = 1 << 30
        let cpu = max(VZVirtualMachineConfiguration.minimumAllowedCPUCount, min(VZVirtualMachineConfiguration.maximumAllowedCPUCount, cores - 2))
        let mem = max(VZVirtualMachineConfiguration.minimumAllowedMemorySize, min(VZVirtualMachineConfiguration.maximumAllowedMemorySize, memory > 8 * gb ? memory - 4 * gb : memory / 2))
        return VMResources(cpuCount: cpu, memoryBytes: mem, diskBytes: diskGB * gb)
    }
}

/// Builds the framework's configuration for a machine on disk.
@MainActor
public enum VMConfiguration {
    public static func make(bundle: VMBundle, resources: VMResources, display: Bool = true) throws -> VZVirtualMachineConfiguration {
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

        // NAT: the machine reaches the internet through the host and is not
        // on the owner's local network as a peer.
        let network = VZVirtioNetworkDeviceConfiguration()
        network.attachment = VZNATNetworkDeviceAttachment()
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

    public init(bundle: VMBundle, resources: VMResources, display: Bool = true) throws {
        self.bundle = bundle
        machine = VZVirtualMachine(configuration: try VMConfiguration.make(bundle: bundle, resources: resources, display: display))
        super.init()
        machine.delegate = self
    }

    public func start() async throws {
        try await machine.start()
    }

    /// Cuts the power. The disk is thrown away after a lease, so there is
    /// nothing to shut down cleanly for.
    public func stop() async {
        guard machine.canStop else { return }
        try? await machine.stop()
    }

    /// The guest's address on the host's NAT network, read from the DHCP
    /// leases macOS keeps. It appears some seconds after the guest boots.
    public func address(timeout: TimeInterval = 180) async throws -> String {
        guard let text = try? String(contentsOf: bundle.macAddress, encoding: .utf8) else { throw VMError.damagedBundle("MAC address") }
        let wanted = Self.normalize(text)
        let deadline = Date().addingTimeInterval(timeout)
        while Date() < deadline {
            if let found = Self.leasedAddress(for: wanted) { return found }
            try await Task.sleep(nanoseconds: 2_000_000_000)
        }
        throw VMError.noAddress
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
