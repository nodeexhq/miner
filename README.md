# NodeEx Miner

The node-side agent of [NodeEx](https://nodeex.com), a network of dedicated Apple-silicon Macs that are rented whole,
one renter at a time. The Miner runs on the Mac you contribute: it checks the hardware, enrolls the machine with the
platform, reports that it is online, and lets a renter in only while they hold a lease on it.

"Mining" is a metaphor for contributing a machine and being paid for real rentals. There is no token and no promised
return: a node earns only when it is rented.

## Status

Early access. The Miner is a command-line program. Release builds are signed with a Developer ID and notarized by
Apple, so macOS runs them without a warning; the first run needs a network connection for that check. There is no
installer, menu-bar app or automatic update yet.

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

While a lease is running, the renter has a login on the machine. Contribute a Mac you do not otherwise use: a new or
freshly erased one is recommended. The Miner has nothing to do with cryptocurrency mining.

## Layout

| Path | What |
| --- | --- |
| `cmd/nodeex-miner` | The program |
| `gate` | Checks lease tokens and forwards the two ports |
| `leasetoken` | The lease token format and its signature check |
| `overlay` | Joining the overlay network |
| `tunnel` | TCP forwarding |
| `internal/` | Device signatures, hardware detection, the minimum requirements |

[NodeEx Connect](https://github.com/nodeexhq/connect), the renter's client, uses the first four.

## Security

Please report vulnerabilities privately: see [SECURITY.md](SECURITY.md).

## Licence

[GNU Affero General Public License v3.0](LICENSE).
