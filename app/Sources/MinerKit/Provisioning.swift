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
            // What the automatic first-run setup left: its trigger and
            // script have done their job and must not reach a renter.
            for leftover in ["private/etc/crontab", "private/var/db/nodeex-firstboot.sh"] {
                try? fm.removeItem(at: data.appendingPathComponent(leftover))
            }
            // A machine that set itself up must have passed its own check.
            // (One set up by hand has neither marker.)
            let db = data.appendingPathComponent("private/var/db")
            if fm.fileExists(atPath: db.appendingPathComponent(".nodeex-firstboot-done").path),
               !fm.fileExists(atPath: db.appendingPathComponent(".nodeex-firstboot-ok").path) {
                throw Failure(description: "the machine's own setup did not pass its check (see /var/log/nodeex-firstboot.log in the machine)")
            }
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

    /// The account the automatic first-run setup makes in the base machine.
    /// It is the host's way in (by key only); its password is random and
    /// kept by nobody. Renters get their own account.
    static let setupAccount = "nodeex"

    /// Where the base machine keeps the installers of the remote desktops
    /// offered to renters (Jump Desktop Connect, UU Remote).
    static let installersDirectory = "/Users/Shared/NodeEx"

    /// The access lists Remote Login and Screen Sharing ask. An account
    /// that is not on them is refused even with the right key or password.
    static let accessLists = "com.apple.access_ssh com.apple.access_screensharing"

    /// What a new machine runs once, as root, at its first boot: it makes
    /// the account, turns on Remote Login and Screen Sharing, marks Setup
    /// Assistant as done, removes its own trigger and shuts down. It logs
    /// to /var/log/nodeex-firstboot.log in the machine.
    static let firstRunScript = """
    #!/bin/sh
    exec >>/private/var/log/nodeex-firstboot.log 2>&1
    [ -e /private/var/db/.nodeex-firstboot-done ] && exit 0
    mkdir /private/var/db/.nodeex-firstboot-lock 2>/dev/null || exit 0
    date
    U=\(setupAccount)
    # Before the commands are echoed to the log: the password must not be
    # written anywhere.
    if ! /usr/bin/id $U >/dev/null 2>&1; then
      /usr/sbin/sysadminctl -addUser $U -fullName NodeEx -admin -password "$(/usr/bin/openssl rand -base64 24)" 2>&1 | /usr/bin/grep -v -i password
    fi
    set -x
    /usr/sbin/createhomedir -c -u $U
    /bin/launchctl enable system/com.openssh.sshd
    /bin/launchctl bootstrap system /System/Library/LaunchDaemons/ssh.plist
    /bin/launchctl enable system/com.apple.screensharing
    /bin/launchctl bootstrap system /System/Library/LaunchDaemons/com.apple.screensharing.plist
    # Turned on this way, each service still asks its access list, which
    # System Settings would have filled in: without it sshd accepts the
    # key and then refuses the account. The account is added by name (a
    # nested administrators group is not honoured); each renter's account
    # is added when it is made.
    for g in \(accessLists); do
      /usr/sbin/dseditgroup -o read $g >/dev/null 2>&1 || /usr/sbin/dseditgroup -o create -q $g
      /usr/sbin/dseditgroup -o edit -a $U -t user $g
    done
    # The renter should not meet an account they do not know at the login
    # window: this one is the host's, and is kept out of sight.
    /usr/bin/dscl . -create /Users/$U IsHidden 1
    # A machine that is rented does not sleep or blank its display (a remote
    # desktop would find it dark), and does not fetch macOS updates on the
    # owner's line or restart itself for them.
    /usr/bin/pmset -a sleep 0 displaysleep 0 disksleep 0
    /usr/bin/defaults write /Library/Preferences/com.apple.SoftwareUpdate AutomaticDownload -bool false
    /usr/bin/defaults write /Library/Preferences/com.apple.SoftwareUpdate AutomaticallyInstallMacOSUpdates -bool false
    /usr/bin/defaults write /Library/Preferences/com.apple.commerce AutoUpdate -bool false
    # Remote desktops that do well on a slow line, for renters who want one:
    # the vendors' own installers, fetched from them and kept only if they
    # carry the vendor's Apple-notarized signature. Each renter's machine
    # installs them when it is made, so no two share a device identity.
    D=\(installersDirectory)
    /bin/mkdir -p $D
    keep() { # file, vendor's team
      if /usr/sbin/pkgutil --check-signature "$1" | /usr/bin/grep -q "($2)" && /usr/sbin/spctl -a -t install "$1"; then
        /bin/chmod 644 "$1"
      else
        /bin/rm -f "$1"
      fi
    }
    if /usr/bin/curl -fsSL --retry 2 -m 240 -o /tmp/jump.dmg https://jumpdesktop.com/downloads/connect/mac &&
       /usr/bin/hdiutil attach -nobrowse -readonly -quiet /tmp/jump.dmg -mountpoint /tmp/jump; then
      /bin/cp /tmp/jump/.jdc.sparkle_guided.pkg "$D/JumpDesktopConnect.pkg" || /bin/cp /tmp/jump/*.pkg "$D/JumpDesktopConnect.pkg"
      /usr/bin/hdiutil detach -quiet /tmp/jump
      keep "$D/JumpDesktopConnect.pkg" 2HCKV38EEC
    fi
    /bin/rm -f /tmp/jump.dmg
    if /usr/bin/curl -fsSL --retry 2 -m 240 -o "$D/UURemote.pkg" "https://api.nrd.nie.163.com/api/v1/release/dl/4?channel=gwqd"; then
      keep "$D/UURemote.pkg" PU9BNSBJW7
    fi
    ls -l $D
    /usr/bin/touch /private/var/db/.AppleSetupDone
    # The crontab cannot be removed from in here (the host wrote it); the
    # host removes it once the machine is off. Until then the marker below
    # keeps this from running twice.
    /usr/bin/touch /private/var/db/.nodeex-firstboot-done
    # The machine checks its own work, and says so only if the host will be
    # able to log in: the account exists, is on both access lists, and
    # Remote Login is loaded. The host does not call the base ready without it.
    ok=1
    /usr/bin/id $U >/dev/null 2>&1 || ok=
    for g in \(accessLists); do
      /usr/sbin/dseditgroup -o checkmember -m $U $g >/dev/null 2>&1 || ok=
    done
    /bin/launchctl print system/com.openssh.sshd >/dev/null 2>&1 || ok=
    if [ -n "$ok" ]; then /usr/bin/touch /private/var/db/.nodeex-firstboot-ok; fi
    /bin/rmdir /private/var/db/.nodeex-firstboot-lock
    /sbin/shutdown -h now

    """

    /// Arranges for a freshly installed, stopped base machine to do its own
    /// first-run setup, so that nobody has to click through Setup Assistant:
    /// the script above goes onto its disk with a system crontab that runs
    /// it (cron starts on its own when /etc/crontab exists, and runs before
    /// anyone has logged in). Returns false, changing nothing, if the
    /// machine has already been set up or has an account.
    @discardableResult
    public static func prepareFirstRun(store: VMStore) throws -> Bool {
        guard store.base.exists else { throw VMError.noBaseImage }
        return try withGuestData(of: store.base) { data in
            let fm = FileManager.default
            let users = ((try? fm.contentsOfDirectory(atPath: data.appendingPathComponent("Users").path)) ?? []).filter { $0 != "Shared" && !$0.hasPrefix(".") }
            guard users.isEmpty, !fm.fileExists(atPath: data.appendingPathComponent("private/var/db/.AppleSetupDone").path) else { return false }
            try firstRunScript.write(to: data.appendingPathComponent("private/var/db/nodeex-firstboot.sh"), atomically: false, encoding: .utf8)
            try "* * * * * root /bin/sh /private/var/db/nodeex-firstboot.sh\n".write(to: data.appendingPathComponent("private/etc/crontab"), atomically: false, encoding: .utf8)
            return true
        }
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
        new=
        if ! /usr/bin/id \(user) >/dev/null 2>&1; then
          sudo -n /usr/sbin/sysadminctl -addUser \(user) -fullName Renter -admin -password "$(/usr/bin/openssl rand -base64 24)" >/dev/null 2>&1
          \(lowBandwidthDefaults(user))
          new=1
        fi
        for g in \(accessLists); do
          if /usr/sbin/dseditgroup -o read $g >/dev/null 2>&1; then sudo -n /usr/sbin/dseditgroup -o edit -a \(user) -t user $g; fi
        done
        sudo -n /bin/mkdir -p /Users/\(user)/.ssh
        sudo -n /usr/bin/tee /Users/\(user)/.ssh/authorized_keys >/dev/null
        sudo -n /usr/sbin/chown -R \(user):staff /Users/\(user)/.ssh
        sudo -n /bin/chmod 700 /Users/\(user)/.ssh
        sudo -n /bin/chmod 600 /Users/\(user)/.ssh/authorized_keys
        # The remote desktops kept in the base are installed for a new
        # renter, once the keys are in: a few seconds each. (Started in the
        # background they died with this session and were never installed.)
        if [ -n "$new" ]; then
          for p in \(installersDirectory)/*.pkg; do
            if [ -f "$p" ]; then sudo -n /usr/sbin/installer -pkg "$p" -target / >/dev/null 2>&1 || true; fi
          done
        fi
        """
        try guest(script, at: endpoint, account: account, store: store, input: keys)
    }

    /// Settings for a new renter account that spare the remote desktop's
    /// bandwidth: renters often connect over slow or lossy lines, and a
    /// machine for development does not need animations or a screen saver.
    /// Each is best effort (the accessibility two are protected on some
    /// macOS versions) and none can stop the account being made.
    static func lowBandwidthDefaults(_ user: String) -> String {
        let writes = [
            "-g NSAutomaticWindowAnimationsEnabled -bool false",
            "-g NSWindowResizeTime -float 0.001",
            "-g NSScrollAnimationEnabled -bool false",
            "-g QLPanelAnimationDuration -float 0",
            "com.apple.dock launchanim -bool false",
            "com.apple.dock expose-animation-duration -float 0.1",
            "com.apple.dock mineffect -string scale",
            "com.apple.finder DisableAllAnimations -bool true",
            "com.apple.universalaccess reduceTransparency -bool true",
            "com.apple.universalaccess reduceMotion -bool true",
        ]
        var lines = ["sudo -n /usr/sbin/createhomedir -c -u \(user) >/dev/null 2>&1 || true"]
        lines += writes.map { "sudo -n -H -u \(user) /usr/bin/defaults write \($0) >/dev/null 2>&1 || true" }
        // No screen saver: -currentHost goes before the verb (after it, as
        // first written, the setting silently went nowhere).
        lines.append("sudo -n -H -u \(user) /usr/bin/defaults -currentHost write com.apple.screensaver idleTime -int 0 >/dev/null 2>&1 || true")
        return lines.joined(separator: "\n  ")
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
