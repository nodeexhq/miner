# NodeEx Miner

The node-side agent of [NodeEx](https://nodeex.com), a network of dedicated Apple-silicon Macs that are rented whole,
one renter at a time. The Miner runs on the Mac you contribute: it checks the hardware, enrolls the machine with the
platform, reports that it is online, and lets a renter in only while they hold a lease on it.

"Mining" is a metaphor for contributing a machine and being paid for real rentals. There is no token and no promised
return: a node earns only when it is rented.

## Status

Early access. The Miner is a command-line program, and the NodeEx Miner app, which sets up the machine and updates
itself, is in testing. Release builds are signed with a Developer ID and notarized by Apple, so macOS runs them
without a warning; the first run needs a network connection for that check.

## Requirements

An Apple-silicon desktop Mac (Mac mini, Mac Studio, iMac, Mac Pro; laptops are refused), macOS 15 or later, 8 CPU
cores, 16 GB of memory, a 256 GB disk with at least 100 GB free. `nodeex-miner check` tests the machine against these
and changes nothing.

## Build

Go 1.27 or later.

```sh
git clone https://github.com/nodeexhq/miner.git
cd miner
go build -o nodeex-miner ./cmd/nodeex-miner
```

## Use

```sh
nodeex-miner check                                   # does this Mac qualify?
nodeex-miner enroll -api https://app.nodeex.com -token CODE
nodeex-miner run                                     # stay online
nodeex-miner service install                         # or: run at login, restart on failure
nodeex-miner status
```

The enroll code comes from the mining dashboard at [app.nodeex.com](https://app.nodeex.com). Every command and flag is
described in the [Miner reference](https://nodeex.com/docs/reference/miner-cli), and the steps from an empty Mac to a
node in service in the [quick start](https://nodeex.com/docs/miners/quickstart).

## What it does on your Mac

- **Keys.** `enroll` generates an Ed25519 device key on the machine. The private key stays in the Miner's state
  directory and is never sent anywhere; every request to the platform is signed with it.
- **Reports.** While running, it sends a heartbeat every 15 seconds: load, free disk, session count, whether the gate
  is open, and the Miner's version. It measures uplink and downlink against an edge location when the platform asks.
- **Network.** It joins an encrypted WireGuard-based overlay. No port is opened on your router.
- **Gate.** It forwards SSH (22) and remote desktop (5900) only for a connection that presents a lease token signed by
  the platform. Tokens last 10 minutes, and access ends when the lease does.
- **Connection test.** Before ordering, a renter's client may test the line to your Mac: a few pings, a download and
  an upload of at most 10 MB each, for at most 10 seconds, admitted by a single-use token from the platform. The Miner
  serves it itself; it never reaches the rented machine, and none runs while a lease is active. All tests together use
  at most 200 MB an hour.

While a lease is running, the renter has a login on the machine. Contribute a Mac you do not otherwise use: a new or
freshly erased one is recommended. The Miner has nothing to do with cryptocurrency mining.

## The macOS app (in development)

`app/` holds a menu-bar app that wraps the command-line Miner. It runs the Miner as a child process, and it is where
per-lease virtual machines are being built: each renter gets a fresh macOS virtual machine, a copy of a base
image. It stops without a valid lease; a different lease replaces it with a new copy. Renters are forwarded to that machine and
never to the owner's own macOS.

This is not finished and is not part of a release yet. It has been run on one Mac: a base machine was installed,
a copy of it booted, SSH and screen sharing reached the guest through the app's ports, and the copy was destroyed and
replaced when a lease ended. The host can now prepare the base machine and install a renter's SSH keys in a
running machine without anyone touching the guest, by writing to the stopped machine's disk and by logging in with a
key only the host holds. What still takes a person is the guest's first-run setup, once per Mac, and the keys do
not yet arrive from the platform. Until the app ships, the command-line Miner forwards connections to the Mac
itself, as before.

```sh
app/scripts/bundle.sh        # builds app/dist/NodeEx Miner.app and app/dist/nodeex-vm
app/dist/nodeex-vm status
app/dist/nodeex-vm install   # download macOS and install the base machine
app/dist/nodeex-vm setup     # boot it in a window: create an account, turn on Remote Login and Screen Sharing, shut down
app/dist/nodeex-vm provision # let the host log in to the base and use sudo there (no window, no clicks)
app/dist/nodeex-vm run       # boot a fresh copy and print its address
```

`NODEEX_VM_CPUS` and `NODEEX_VM_MEMORY_GB` limit what a machine is given; by default it gets all but two cores and
all but 4 GB of memory, which suits a Mac that does nothing else.

The app updates itself with [Sparkle](https://sparkle-project.org). It reads `app/appcast.xml` from this
repository, accepts only an update signed with the key built into the app and with the same Developer ID, and
installs it with a restart when no lease is running: an update never cuts a renter off.

A renter's machine is not on the owner's network. Its network device is a socket held by `vmnet/`, a small
user-space network that runs without root: it gives the machine an address, DNS and the internet, and refuses
everything addressed to private ranges (the owner's local network, this Mac, the overlay) and to the mail ports 25,
465 and 587. The rules are applied on the host, outside the machine, so the machine's administrator cannot lift
them. `vmnet/policy.go` is the whole policy.

`nodeex-miner run -events` writes state changes (node status, gate, lease started and ended) to standard output as
JSON lines; that is how the app follows the Miner.

### VM lease enforcement

The macOS app controls the guest's power and its only network connection on the host. Jump Desktop, UU Remote,
and other services installed **inside the renter's VM** therefore follow the same lease even when they bypass
the NodeEx SSH/VNC gate. This does not manage remote access installed on the host. Prepare the base without
renter accounts, initialized Jump/UU device identities, or remote-access pairing: install/register those agents
inside each renter's guest so a new disk cannot inherit an earlier device identity or unattended password.

- **Expiry:** the app checks the last confirmed end time every second. At the deadline it holds the VM's network
  and pauses the guest, even if the Go core or platform is unavailable. The gate's token clock grace does not
  extend VM runtime.
- **Lease ended:** the platform reports a lease until it is settled, including one past its end that may still
  be renewed, so a successful heartbeat confirming no lease means it ended (expired, terminated early, or
  abandoned). The old guest is powered off and its disk deleted: nothing of the renter's stays on the owner's
  Mac. It never starts an idle guest. Heartbeats normally run every 15 seconds; an early termination is observed
  on the next successful reply, not instantaneously.
- **Renewal:** every successful heartbeat includes the lease ID, current deadline, and full SSH key set. A renewed
  deadline for the same lease does not restart or reset its guest. A different lease always gets a clean copy.
- **Restart:** neither the app nor the host restarting boots a saved renter disk before a fresh platform
  confirmation. The same active lease resumes its own disk; a different lease replaces it; no lease deletes it.
- **Lost confirmation:** a core exit, host sleep, a lease past its end, or 180 seconds without a valid successful
  heartbeat holds the network and pauses the guest, memory and all. HTTP error replies do not extend this VM
  authorization. A fresh confirmation of the same lease resumes it where it was; a guest paused for 30 minutes,
  or one that cannot be paused (still starting or stopping), is powered off with its disk kept.

Held means the network helper drops every frame both ways; the paused guest sends nothing either. Stale, empty,
or incomplete replies cannot authorize a start or count as proof that there is no lease. Pauses, power-off,
retries/reboots, and pending starts are serialized before any disk replacement or deletion. Both wall time and
elapsed uptime bound authorization, so a backward host clock correction cannot extend a saved deadline. These
guarantees apply to VMs managed by the menu-bar app, not a VM manually started with the development
`nodeex-vm run` command.

Before releasing, test with a disposable renter VM: keep actual Jump/UU sessions open, expire or terminate the
lease and verify both disconnect and the VM's directory is gone once the platform settles the lease; check that
same-lease renewal preserves a file and avoids a reboot; stop the platform (or the core) for a minute and check
that the guest pauses and resumes with its programs still running; leave it unconfirmed past 30 minutes and check
it powers off and a later same-lease confirmation boots the same disk; restart Miner with the platform
unreachable and check the old VM stays off; then confirm a different lease and check that the file and old
remote-access pairing are absent. Automated tests use a fake VM and temporary disk markers; they do not boot the
owner's VM or exercise third-party desktop clients.

## Layout

| Path | What |
| --- | --- |
| `cmd/nodeex-miner` | The program |
| `gate` | Checks lease tokens and forwards the two ports |
| `leasetoken` | The lease and test token format and its signature check |
| `pretest` | The pre-order connection test: its protocol, client and server, and who may run one |
| `overlay` | Joining the overlay network |
| `tunnel` | TCP forwarding |
| `internal/` | Device signatures, hardware detection, the minimum requirements |
| `app/` | The macOS menu-bar app and virtual machines (Swift, in development) |
| `vmnet/` | The network of a renter's machine and its outbound rules (Go, a module of its own) |

[NodeEx Connect](https://github.com/nodeexhq/connect), the renter's client, uses the first four.

## Security

Official builds are published only on this repository's releases page and on https://nodeex.com/download. They are
signed with NodeEx's Developer ID (team ID `7GS265YM4T`) and notarized by Apple, and every release has a `SHA256SUMS`
file. Before running a build, check it:

```sh
shasum -a 256 -c SHA256SUMS --ignore-missing
spctl -a -vv -t install NodeEx-Miner-vX.Y.Z.dmg                 # source=Notarized Developer ID
codesign -dv NodeEx-Miner-vX.Y.Z.dmg 2>&1 | grep TeamIdentifier # TeamIdentifier=7GS265YM4T
```

A fork of this code is not an official build, whatever it is called, and cannot carry that signature. See
[how to check a download](https://nodeex.com/security#software).

Please report vulnerabilities privately: see [SECURITY.md](SECURITY.md).

## Licence

[GNU Affero General Public License v3.0](LICENSE).
