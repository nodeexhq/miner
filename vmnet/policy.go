package main

import (
	"encoding/binary"
	"log"
	"net"
	"net/netip"
	"slices"
	"sync/atomic"
	"time"
)

// What a rented machine may not reach.
//
// Private and special-purpose addresses cover the owner's local network,
// this Mac, other machines on the overlay (100.64.0.0/10) and anything else
// that is not the public internet. The machine's own small network, where
// its gateway and DNS live, is the one exception.
var refusedNets = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("10.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("127.0.0.0/8"),
	netip.MustParsePrefix("169.254.0.0/16"),
	netip.MustParsePrefix("172.16.0.0/12"),
	netip.MustParsePrefix("192.0.0.0/24"),
	netip.MustParsePrefix("192.168.0.0/16"),
	netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("224.0.0.0/3"), // multicast and reserved
}

// Mail submission and relay: a rented machine is not a mail server, and
// one node sending spam would get every owner's address blocked.
var refusedTCPPorts = map[uint16]bool{25: true, 465: true, 587: true}

const (
	etherIPv4 = 0x0800
	etherARP  = 0x0806
	protoTCP  = 6
	protoUDP  = 17
)

// allowed reports whether an Ethernet frame sent by the machine may go on.
// guestNet is the machine's own network.
// home lists the owner's own public addresses, refused like the owner's
// local network.
func allowed(frame []byte, guestNet netip.Prefix, home []netip.Addr) bool {
	if len(frame) < 14 {
		return false
	}
	switch binary.BigEndian.Uint16(frame[12:14]) {
	case etherARP:
		return true
	case etherIPv4:
	default:
		// The network is IPv4 only; IPv6 would have nowhere to go, and
		// refusing it outright leaves no second path to think about.
		return false
	}
	ip := frame[14:]
	if len(ip) < 20 || ip[0]>>4 != 4 {
		return false
	}
	ihl := int(ip[0]&0x0f) * 4
	if ihl < 20 || len(ip) < ihl {
		return false
	}
	dst := netip.AddrFrom4([4]byte(ip[16:20]))
	if dst == netip.AddrFrom4([4]byte{255, 255, 255, 255}) {
		// DHCP, and only that.
		return ip[9] == protoUDP && len(ip) >= ihl+4 && binary.BigEndian.Uint16(ip[ihl+2:ihl+4]) == 67
	}
	if slices.Contains(home, dst) {
		return false
	}
	if !guestNet.Contains(dst) {
		for _, n := range refusedNets {
			if n.Contains(dst) {
				return false
			}
		}
	}
	// Only a packet's first fragment carries its ports; later fragments of
	// a refused packet are useless without it.
	fragment := binary.BigEndian.Uint16(ip[6:8]) & 0x1fff
	if ip[9] == protoTCP && fragment == 0 {
		if len(ip) < ihl+4 {
			return false
		}
		if refusedTCPPorts[binary.BigEndian.Uint16(ip[ihl+2:ihl+4])] {
			return false
		}
	}
	return true
}

// filteredConn drops the frames the policy refuses before the virtual
// network sees them. Frames going to the machine are not filtered: they are
// replies to what it was allowed to send (the abuse watch only looks at
// them, to see which connection attempts were answered).
type filteredConn struct {
	net.Conn
	guestNet netip.Prefix
	home     *homeAddrs
	gateway  netip.Addr
	// held drops every frame both ways while the machine's lease is not
	// confirmed; the machine is paused then, and this covers the moment
	// before it is.
	held     atomic.Bool
	up, down bucket
	dropped  atomic.Uint64
	// Frames dropped (out) or made to wait (in) by the bandwidth limits.
	limitedUp, limitedDown atomic.Uint64
	// abuse watches for scanning and floods (abuse.go); once it trips,
	// every frame is dropped both ways, as when held, for good. Nil is no
	// watch.
	abuse *abuseGuard
}

// cut reports whether every frame is dropped: the lease is not confirmed,
// or the machine was caught abusing the owner's address.
func (c *filteredConn) cut() bool { return c.held.Load() || c.abuse.isTripped() }

// leaseHold applies the app's hold (SIGUSR1) or release (SIGUSR2), for
// lease reasons, and reports whether that changed anything.
//
// A cut for abuse is deliberately not part of held: it is sticky, and a
// release does not undo it. The app releases the network whenever the
// lease is confirmed again (after a pause, a restart of the core, a late
// renewal), which says nothing about what the machine did; letting that
// reopen the network would hand it straight back to whoever was scanning
// from it. Only a new helper, which comes with a new machine, starts
// clean. Ending the lease and telling the platform is the app's to do,
// from the event on standard output.
func (c *filteredConn) leaseHold(held bool) bool {
	changed := c.held.Swap(held) != held
	if changed {
		log.Printf("network held: %v", held)
	}
	if !held && c.abuse.isTripped() {
		log.Printf("network stays cut for abuse: a release does not lift it")
	}
	return changed
}

func (c *filteredConn) Read(b []byte) (int, error) {
	for {
		n, err := c.Conn.Read(b)
		if err != nil {
			return n, err
		}
		if c.cut() {
			continue
		}
		if !allowed(b[:n], c.guestNet, c.home.list()) {
			c.dropped.Add(1)
			continue
		}
		if !c.abuse.out(b[:n], time.Now()) {
			continue
		}
		if metered(b[:n], true, c.gateway) {
			wait, ok := c.up.pace(n, time.Now(), upQueue)
			if !ok {
				c.limitedUp.Add(1)
				continue
			}
			time.Sleep(wait)
		}
		return n, nil
	}
}

// Write carries a frame to the machine, when the limit has room for it.
func (c *filteredConn) Write(b []byte) (int, error) {
	if c.cut() {
		return len(b), nil
	}
	c.abuse.in(b, time.Now())
	if metered(b, false, c.gateway) {
		if d := c.down.delay(len(b), time.Now()); d > 0 {
			c.limitedDown.Add(1)
			time.Sleep(d)
		}
	}
	return c.Conn.Write(b)
}

func mustCIDR(s string) netip.Prefix { return netip.MustParsePrefix(s) }
