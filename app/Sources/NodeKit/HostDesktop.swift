import Darwin
import Foundation
import ObjectiveC
import Security
import Virtualization

/// The machine's screen, keyboard and mouse served from the host by
/// Virtualization's own VNC server, in place of the guest's Screen Sharing.
///
/// The guest's Screen Sharing cannot be used on a machine nobody has sat in
/// front of: since macOS 15 it needs the screen-recording and accessibility
/// grants that only System Settings (or MDM) can make, and without them it
/// accepts the password and then refuses to show the screen. The host has
/// the machine's display already; the framework can serve it over RFB.
///
/// That server is not public API (_VZVNCServer). It is reached through the
/// Objective-C runtime, each class and method checked with its type before
/// use, so a macOS without it, or with a different shape of it, builds and
/// runs the same and simply keeps the guest's Screen Sharing.
///
/// The server listens on every interface and has no option to choose one.
/// Once it is up, its listening socket is bound to the loopback interface
/// (IP_BOUND_IF), so only this Mac reaches it. If that cannot be done the
/// server is stopped again.
///
/// Renters never reach it directly. A viewer that sends SetEncodings
/// without the DesktopSize pseudo-encoding makes the server stop this whole
/// process on an internal assertion, machine and all; so the gate's remote
/// desktop port is the network helper's, which passes each connection on
/// through a filter that adds it (vmnet/rfb.go).
///
/// It offers only VNC authentication (RFB security type 2), whose DES
/// challenge uses the first eight characters of the password. So the
/// password is eight random characters, made for each lease's machine.
@MainActor
public final class HostDesktop {
    public struct Config: Sendable, Equatable {
        /// The loopback port to serve on.
        public let port: UInt16
        public let password: String

        public init(port: UInt16, password: String) {
            self.port = port
            self.password = password
        }
    }

    public enum Failure: Error, CustomStringConvertible {
        case unavailable(String)
        case notStarted(String)
        case notRestricted

        public var description: String {
            switch self {
            case .unavailable(let why): return "the host's VNC server is not available on this macOS: \(why)"
            case .notStarted(let why): return "the host's VNC server did not start: \(why)"
            case .notRestricted: return "the host's VNC server could not be limited to this Mac; it was stopped"
            }
        }
    }

    // What is used of the private classes, with the type encodings this
    // code was written against. A method with another signature is treated
    // as missing: calling it with the wrong arguments would crash.
    nonisolated static let serverClass = "_VZVNCServer"
    nonisolated static let securityClass = "_VZVNCAuthenticationSecurityConfiguration"
    nonisolated static let initServer = "initWithPort:queue:securityConfiguration:"
    nonisolated static let initSecurity = "initWithPassword:"
    nonisolated static let expected: [(cls: String, selector: String, types: String)] = [
        (securityClass, initSecurity, "@24@0:8@16"),
        (serverClass, initServer, "@36@0:8S16@20@28"),
        (serverClass, "setVirtualMachine:", "v24@0:8@16"),
        (serverClass, "start", "v16@0:8"),
        (serverClass, "stop", "v16@0:8"),
        (serverClass, "port", "S16@0:8"),
        (serverClass, "state", "q16@0:8"),
    ]

    /// Nil when the server can be used here; otherwise why not.
    /// NVTO_HOST_VNC=0 turns it off, which leaves the guest's own Screen
    /// Sharing as before.
    public nonisolated static var unavailableReason: String? {
        if ProcessInfo.processInfo.environment["NVTO_HOST_VNC"] == "0" { return "turned off by NVTO_HOST_VNC=0" }
        // The framework is loaded by the first use of any of its classes.
        _ = VZVirtualMachineConfiguration.self
        for (name, selector, types) in expected {
            guard let cls = NSClassFromString(name) else { return "\(name) is missing" }
            guard let method = class_getInstanceMethod(cls, NSSelectorFromString(selector)) else { return "\(name) has no \(selector)" }
            let found = method_getTypeEncoding(method).map { String(cString: $0) } ?? ""
            guard found == types else { return "\(name) \(selector) has type \(found), not \(types)" }
        }
        return nil
    }

    public nonisolated static var isAvailable: Bool { unavailableReason == nil }

    /// The port it serves on.
    public let port: UInt16
    private let server: NSObject
    private var stopped = false

    private init(server: NSObject, port: UInt16) {
        self.server = server
        self.port = port
    }

    /// Starts serving machine's display on 127.0.0.1:config.port. A port
    /// still held (by the server of the machine before, or the guest
    /// network of an older app) is waited for, up to timeout. Without a
    /// machine (tests) the server runs with no display behind it.
    public static func start(_ config: Config, machine: VZVirtualMachine?, timeout: TimeInterval = 15) async throws -> HostDesktop {
        if let why = unavailableReason { throw Failure.unavailable(why) }
        let deadline = Date().addingTimeInterval(timeout)
        while true {
            let server = try make(config)
            if let machine { call(server, "setVirtualMachine:", object: machine) }
            call(server, "start")
            // The port is reported once the server is listening; a port in
            // use leaves it at 0 with the state failed (2).
            var port: UInt16 = 0
            for _ in 0..<20 {
                port = portOf(server)
                if port != 0 || stateOf(server) == 2 { break }
                try await Task.sleep(nanoseconds: 100_000_000)
            }
            if port == config.port {
                // Until this returns the port is open on every interface,
                // behind the password, for a few milliseconds.
                guard restrictToLoopback(port: port) > 0 else {
                    call(server, "stop")
                    throw Failure.notRestricted
                }
                return HostDesktop(server: server, port: port)
            }
            call(server, "stop")
            if port != 0 { throw Failure.notStarted("it took port \(port), not \(config.port)") }
            if Date() > deadline { throw Failure.notStarted("port \(config.port) stayed in use (state \(stateOf(server)))") }
            try await Task.sleep(nanoseconds: 250_000_000)
        }
    }

    /// Stops serving; connected viewers are cut off. It cannot be started
    /// again: start a new one.
    public func stop() {
        guard !stopped else { return }
        stopped = true
        Self.call(server, "stop")
    }

    /// A password for a machine's remote desktop: eight characters (all
    /// VNC authentication uses) without the ones easy to confuse, about 46
    /// bits. Only a lease's token gets a connection through the gate to it.
    public nonisolated static func newPassword() -> String {
        let alphabet = Array("abcdefghjkmnpqrstuvwxyzABCDEFGHJKLMNPQRSTUVWXYZ23456789")
        var bytes = [UInt8](repeating: 0, count: 8)
        if SecRandomCopyBytes(kSecRandomDefault, bytes.count, &bytes) != errSecSuccess {
            // Not expected; arc4random is also a system CSPRNG.
            bytes = bytes.map { _ in UInt8.random(in: 0...255) }
        }
        // 54 symbols: the slight bias of the modulo is of no account here.
        return String(bytes.map { alphabet[Int($0) % alphabet.count] })
    }

    /// The password of the machine in bundle, made the first time. It is
    /// kept in the machine's own directory, so it lasts as long as the
    /// lease's machine and goes with it; a fresh copy gets a new one.
    public nonisolated static func password(for bundle: VMBundle) throws -> String {
        let file = bundle.desktopPassword
        if let text = try? String(contentsOf: file, encoding: .utf8), text.count == 8 { return text }
        let password = newPassword()
        FileManager.default.createFile(atPath: file.path, contents: Data(password.utf8), attributes: [.posixPermissions: 0o600])
        guard (try? String(contentsOf: file, encoding: .utf8)) == password else { throw VMError.damagedBundle("remote desktop password") }
        return password
    }

    // MARK: - The Objective-C runtime

    private typealias AllocIMP = @convention(c) (AnyClass, Selector) -> UnsafeMutableRawPointer?
    private typealias InitSecurityIMP = @convention(c) (UnsafeMutableRawPointer, Selector, NSString) -> UnsafeMutableRawPointer?
    private typealias InitServerIMP = @convention(c) (UnsafeMutableRawPointer, Selector, UInt16, DispatchQueue, AnyObject) -> UnsafeMutableRawPointer?
    private typealias VoidIMP = @convention(c) (AnyObject, Selector) -> Void
    private typealias ObjectIMP = @convention(c) (AnyObject, Selector, AnyObject) -> Void
    private typealias PortIMP = @convention(c) (AnyObject, Selector) -> UInt16
    private typealias StateIMP = @convention(c) (AnyObject, Selector) -> Int

    private static func imp<T>(_ cls: AnyClass, _ selector: String, as: T.Type) -> T {
        unsafeBitCast(class_getMethodImplementation(cls, NSSelectorFromString(selector)), to: T.self)
    }

    /// alloc and init by hand, with their ownership: alloc's object is
    /// consumed by init, and what init returns is owned by the caller.
    private static func alloc(_ cls: AnyClass) -> UnsafeMutableRawPointer? {
        let alloc = NSSelectorFromString("alloc")
        let f = unsafeBitCast(method_getImplementation(class_getClassMethod(cls, alloc)!), to: AllocIMP.self)
        return f(cls, alloc)
    }

    private static func make(_ config: Config) throws -> NSObject {
        guard let securityCls = NSClassFromString(securityClass), let serverCls = NSClassFromString(serverClass),
              let rawSecurity = alloc(securityCls), let rawServer = alloc(serverCls) else { throw Failure.unavailable("could not allocate") }
        let initSec = imp(securityCls, initSecurity, as: InitSecurityIMP.self)
        guard let security = initSec(rawSecurity, NSSelectorFromString(initSecurity), config.password as NSString) else {
            throw Failure.unavailable("the security configuration was refused")
        }
        let securityObject = Unmanaged<AnyObject>.fromOpaque(security).takeRetainedValue()
        let initSrv = imp(serverCls, initServer, as: InitServerIMP.self)
        let queue = DispatchQueue(label: "nvto.host-desktop")
        guard let server = initSrv(rawServer, NSSelectorFromString(initServer), config.port, queue, securityObject) else {
            throw Failure.unavailable("the server was refused")
        }
        guard let object = Unmanaged<AnyObject>.fromOpaque(server).takeRetainedValue() as? NSObject else {
            throw Failure.unavailable("the server is not an object")
        }
        return object
    }

    private static func call(_ server: NSObject, _ selector: String) {
        imp(type(of: server), selector, as: VoidIMP.self)(server, NSSelectorFromString(selector))
    }

    private static func call(_ server: NSObject, _ selector: String, object: AnyObject) {
        imp(type(of: server), selector, as: ObjectIMP.self)(server, NSSelectorFromString(selector), object)
    }

    private static func portOf(_ server: NSObject) -> UInt16 {
        imp(type(of: server), "port", as: PortIMP.self)(server, NSSelectorFromString("port"))
    }

    private static func stateOf(_ server: NSObject) -> Int {
        imp(type(of: server), "state", as: StateIMP.self)(server, NSSelectorFromString("state"))
    }

    // MARK: - Loopback only

    /// Binds every listening socket of this process on port to the
    /// loopback interface, so that connections arriving on any other
    /// interface are dropped by the kernel. Returns how many were bound and
    /// read back as bound; 0 means the server could not be found.
    nonisolated static func restrictToLoopback(port: UInt16) -> Int {
        let loopback = UInt32(if_nametoindex("lo0"))
        guard loopback != 0 else { return 0 }
        var restricted = 0
        for fd in Int32(0)..<Int32(getdtablesize()) {
            var local = sockaddr_storage()
            var length = socklen_t(MemoryLayout<sockaddr_storage>.size)
            let named = withUnsafeMutablePointer(to: &local) { $0.withMemoryRebound(to: sockaddr.self, capacity: 1) { getsockname(fd, $0, &length) } }
            guard named == 0, localPort(local) == port else { continue }
            // A connected socket on the same port is a viewer, not the
            // listener.
            var peer = sockaddr_storage()
            var peerLength = socklen_t(MemoryLayout<sockaddr_storage>.size)
            let connected = withUnsafeMutablePointer(to: &peer) { $0.withMemoryRebound(to: sockaddr.self, capacity: 1) { getpeername(fd, $0, &peerLength) } }
            guard connected != 0 else { continue }
            // On an IPv6 socket the IPv6 option also covers IPv4 arriving
            // on it (dual stack).
            let (level, option) = local.ss_family == sa_family_t(AF_INET6) ? (IPPROTO_IPV6, IPV6_BOUND_IF) : (IPPROTO_IP, IP_BOUND_IF)
            var index = loopback
            guard setsockopt(fd, level, option, &index, socklen_t(MemoryLayout<UInt32>.size)) == 0 else { continue }
            var check: UInt32 = 0
            var checkLength = socklen_t(MemoryLayout<UInt32>.size)
            if getsockopt(fd, level, option, &check, &checkLength) == 0, check == loopback { restricted += 1 }
        }
        return restricted
    }

    nonisolated static func localPort(_ address: sockaddr_storage) -> UInt16? {
        var address = address
        switch Int32(address.ss_family) {
        case AF_INET6: return withUnsafePointer(to: &address) { $0.withMemoryRebound(to: sockaddr_in6.self, capacity: 1) { UInt16(bigEndian: $0.pointee.sin6_port) } }
        case AF_INET: return withUnsafePointer(to: &address) { $0.withMemoryRebound(to: sockaddr_in.self, capacity: 1) { UInt16(bigEndian: $0.pointee.sin_port) } }
        default: return nil
        }
    }
}
