import Foundation

/// What the host does to a machine without anyone sitting in front of it.
///
/// Two channels are used. A machine that is not running can have its disk
/// mounted on the host, and files written into it: that is how the base
/// machine is prepared. A machine that is running is reached over SSH with a
/// key only this host holds, which the preparation put into the guest: that
/// is how each renter's keys are installed.
public enum Provisioning {
    public struct Failure: Error, CustomStringConvertible {
        public let description: String
    }

    /// The host's own key for logging in to its machines. It never leaves
    /// this Mac and is not the renter's key.
    public static func hostKey(store: VMStore) throws -> URL {
        let key = store.root.appendingPathComponent("provision_key")
        if !FileManager.default.fileExists(atPath: key.path) {
            try FileManager.default.createDirectory(at: store.root, withIntermediateDirectories: true)
            try run("/usr/bin/ssh-keygen", ["-q", "-t", "ed25519", "-N", "", "-C", "nodeex-host", "-f", key.path])
        }
        return key
    }

    /// The line that lets the guest's administrators use sudo without a
    /// password. The renter is the administrator of a machine that exists
    /// only for them, and the host needs it to configure the guest.
    static let sudoRule = "%admin ALL=(ALL) NOPASSWD: ALL"

    /// Prepares a stopped base machine: the host's key goes into the
    /// account's authorized_keys and administrators get password-less sudo.
    /// It can be run again; it changes nothing that is already in place.
    /// Returns the name of the guest account.
    @discardableResult
    public static func prepareBase(store: VMStore) throws -> String {
        guard store.base.exists else { throw VMError.noBaseImage }
        let publicKey = try String(contentsOf: try hostKey(store: store).appendingPathExtension("pub"), encoding: .utf8).trimmingCharacters(in: .whitespacesAndNewlines)
        let account: String = try withGuestData(of: store.base) { data in
            let fm = FileManager.default
            let users = try fm.contentsOfDirectory(atPath: data.appendingPathComponent("Users").path).filter { $0 != "Shared" && !$0.hasPrefix(".") }
            guard users.count == 1, let account = users.first else {
                throw Failure(description: "expected one account in the base machine, found \(users.sorted())")
            }
            let ssh = data.appendingPathComponent("Users/\(account)/.ssh")
            try fm.createDirectory(at: ssh, withIntermediateDirectories: true, attributes: [.posixPermissions: 0o700])
            let authorized = ssh.appendingPathComponent("authorized_keys")
            let existing = (try? String(contentsOf: authorized, encoding: .utf8)) ?? ""
            if !existing.contains(publicKey) {
                try (existing + (existing.isEmpty || existing.hasSuffix("\n") ? "" : "\n") + publicKey + "\n").write(to: authorized, atomically: false, encoding: .utf8)
            }
            try fm.setAttributes([.posixPermissions: 0o600], ofItemAtPath: authorized.path)

            // sudoers must stay owned by root, so it is edited in place: a
            // file the host created would belong to the wrong user and sudo
            // would refuse it.
            let sudoers = data.appendingPathComponent("private/etc/sudoers")
            let rules = try String(contentsOf: sudoers, encoding: .utf8)
            if !rules.contains(sudoRule) {
                // The file is read-only even for its owner; sudo insists on
                // that mode, so it is put back afterwards.
                try fm.setAttributes([.posixPermissions: 0o640], ofItemAtPath: sudoers.path)
                defer { try? fm.setAttributes([.posixPermissions: 0o440], ofItemAtPath: sudoers.path) }
                let handle = try FileHandle(forWritingTo: sudoers)
                defer { try? handle.close() }
                try handle.seekToEnd()
                try handle.write(contentsOf: Data(((rules.hasSuffix("\n") ? "" : "\n") + sudoRule + "\n").utf8))
            }
            return account
        }
        try account.write(to: accountFile(store: store), atomically: true, encoding: .utf8)
        return account
    }

    static func accountFile(store: VMStore) -> URL { store.root.appendingPathComponent("account") }

    /// The guest account the base machine was prepared with, or nil if it
    /// has not been prepared.
    public static func account(store: VMStore) -> String? {
        (try? String(contentsOf: accountFile(store: store), encoding: .utf8))?.trimmingCharacters(in: .whitespacesAndNewlines)
    }

    /// Mounts the data volume of a stopped machine's disk on the host for
    /// the length of the closure.
    static func withGuestData<T>(of bundle: VMBundle, _ body: (URL) throws -> T) throws -> T {
        let attached = try run("/usr/bin/hdiutil", ["attach", "-nomount", "-imagekey", "diskimage-class=CRawDiskImage", bundle.disk.path])
        guard let whole = attached.split(separator: "\n").first?.split(whereSeparator: { $0 == " " || $0 == "\t" }).first.map(String.init) else {
            throw Failure(description: "could not attach \(bundle.disk.path)")
        }
        defer { _ = try? run("/usr/bin/hdiutil", ["detach", whole]) }
        guard let store = attached.split(separator: "\n").first(where: { $0.contains("Apple_APFS ") })?.split(whereSeparator: { $0 == " " || $0 == "\t" }).first.map({ $0.replacingOccurrences(of: "/dev/", with: "") }) else {
            throw Failure(description: "the disk has no macOS container")
        }
        // The volume with the Data role, in the container on that store.
        let listing = try run("/usr/sbin/diskutil", ["apfs", "list"])
        var inContainer = false
        var volume: String?
        for line in listing.split(separator: "\n") {
            if line.contains("APFS Physical Store Disk") { inContainer = line.contains(store) }
            if inContainer, line.contains("(Data)"), let name = line.split(separator: " ").dropLast().last { volume = String(name) }
        }
        guard let volume else { throw Failure(description: "the disk has no data volume") }
        let mountPoint = FileManager.default.temporaryDirectory.appendingPathComponent("nodeex-guest-\(UUID().uuidString)")
        try FileManager.default.createDirectory(at: mountPoint, withIntermediateDirectories: true)
        defer { try? FileManager.default.removeItem(at: mountPoint) }
        try run("/usr/sbin/diskutil", ["mount", "-mountPoint", mountPoint.path, volume])
        defer { _ = try? run("/usr/sbin/diskutil", ["unmount", mountPoint.path]) }
        return try body(mountPoint)
    }

    /// Runs a command in a running guest as its account, feeding it input,
    /// and returns what it printed.
    @discardableResult
    public static func guest(_ command: String, at endpoint: VMRunner.Endpoint, account: String, store: VMStore, input: String = "") throws -> String {
        try run("/usr/bin/ssh", [
            "-i", try hostKey(store: store).path, "-o", "IdentitiesOnly=yes", "-o", "BatchMode=yes",
            "-o", "StrictHostKeyChecking=no", "-o", "UserKnownHostsFile=/dev/null", "-o", "LogLevel=ERROR", "-o", "ConnectTimeout=10",
            "-p", String(endpoint.port), "\(account)@\(endpoint.host)", command,
        ], input: input)
    }

    /// The account a renter gets in the machine. It is made for the lease,
    /// apart from the account the host manages the machine with, so the
    /// renter can be given its password without learning the host's.
    public static let renterAccount = "renter"

    /// Makes exactly these keys the renter's way in to a running machine,
    /// creating the renter's account the first time. An empty list leaves
    /// the account with no keys: only the host can log in.
    ///
    /// The account starts with a random password nobody keeps. A new
    /// account's password can be reset by an administrator, which is how
    /// the renter's own client gives it one for remote desktop.
    public static func authorize(renterKeys: [String], at endpoint: VMRunner.Endpoint, account: String, store: VMStore) throws {
        let keys = renterKeys.map { $0.trimmingCharacters(in: .whitespacesAndNewlines) }.filter(isPublicKey).joined(separator: "\n") + "\n"
        let user = renterAccount
        let script = """
        set -e
        if ! /usr/bin/id \(user) >/dev/null 2>&1; then
          sudo -n /usr/sbin/sysadminctl -addUser \(user) -fullName Renter -admin -password "$(/usr/bin/openssl rand -base64 24)" >/dev/null 2>&1
        fi
        sudo -n /bin/mkdir -p /Users/\(user)/.ssh
        sudo -n /usr/bin/tee /Users/\(user)/.ssh/authorized_keys >/dev/null
        sudo -n /usr/sbin/chown -R \(user):staff /Users/\(user)/.ssh
        sudo -n /bin/chmod 700 /Users/\(user)/.ssh
        sudo -n /bin/chmod 600 /Users/\(user)/.ssh/authorized_keys
        """
        try guest(script, at: endpoint, account: account, store: store, input: keys)
    }

    /// A single line that looks like an OpenSSH public key, so that nothing
    /// else can be smuggled into authorized_keys.
    static func isPublicKey(_ line: String) -> Bool {
        let parts = line.split(separator: " ", omittingEmptySubsequences: true)
        guard parts.count >= 2, !line.contains("\n"), !line.contains("\r") else { return false }
        let kinds = ["ssh-ed25519", "ssh-rsa", "ecdsa-sha2-nistp256", "ecdsa-sha2-nistp384", "ecdsa-sha2-nistp521", "sk-ssh-ed25519@openssh.com", "sk-ecdsa-sha2-nistp256@openssh.com"]
        return kinds.contains(String(parts[0])) && Data(base64Encoded: String(parts[1])) != nil
    }

    @discardableResult
    static func run(_ tool: String, _ arguments: [String], input: String = "") throws -> String {
        let p = Process()
        p.executableURL = URL(fileURLWithPath: tool)
        p.arguments = arguments
        let out = Pipe(), err = Pipe(), stdin = Pipe()
        p.standardOutput = out
        p.standardError = err
        p.standardInput = stdin
        try p.run()
        stdin.fileHandleForWriting.write(Data(input.utf8))
        try? stdin.fileHandleForWriting.close()
        let output = out.fileHandleForReading.readDataToEndOfFile()
        let errors = err.fileHandleForReading.readDataToEndOfFile()
        p.waitUntilExit()
        guard p.terminationStatus == 0 else {
            throw Failure(description: "\(URL(fileURLWithPath: tool).lastPathComponent) failed: \(String(data: errors, encoding: .utf8)?.trimmingCharacters(in: .whitespacesAndNewlines) ?? "status \(p.terminationStatus)")")
        }
        return String(data: output, encoding: .utf8) ?? ""
    }
}
