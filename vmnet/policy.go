package main

import (
	"encoding/binary"
	"net"
	"net/netip"
	"sync/atomic"
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
func allowed(frame []byte, guestNet netip.Prefix) bool {
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
// network sees them. Frames going to the machine are not examined: they are
// replies to what it was allowed to send.
type filteredConn struct {
	net.Conn
	guestNet netip.Prefix
	dropped  atomic.Uint64
}

func (c *filteredConn) Read(b []byte) (int, error) {
	for {
		n, err := c.Conn.Read(b)
		if err != nil {
			return n, err
		}
		if allowed(b[:n], c.guestNet) {
			return n, nil
		}
		c.dropped.Add(1)
	}
}

func mustCIDR(s string) netip.Prefix { return netip.MustParsePrefix(s) }
