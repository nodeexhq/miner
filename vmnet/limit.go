package main

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"log"
	"net/netip"
	"os"
	"sync"
	"time"
)

// A rented machine may not take the owner's whole line: its traffic is held
// to the limits the platform derives from the measured line (most of it,
// the rest left to the household). The remote desktop is never held back:
// it is what a renter feels first, and it is light. Everything else counts,
// including SSH, since a copy over SSH uses the owner's line like any other.
//
// The limit polices rather than queues: a frame over the limit is dropped
// and TCP slows down, so nothing waits behind a bulk transfer, the remote
// desktop least of all.

const remoteDesktopPort = 5900

// bucket is a token bucket counting bytes; a zero rate means no limit.
type bucket struct {
	mu     sync.Mutex
	rate   float64 // bytes per second
	burst  float64
	tokens float64
	last   time.Time
}

func (b *bucket) set(mbps float64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.rate = mbps * 1e6 / 8
	// A quarter of a second of traffic, and never less than a few full
	// windows, so that TCP can still reach the limit.
	b.burst = max(b.rate/4, 256<<10)
	b.tokens = min(b.tokens, b.burst)
}

func (b *bucket) allow(n int, now time.Time) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.rate <= 0 {
		return true
	}
	if !b.last.IsZero() {
		b.tokens = min(b.burst, b.tokens+now.Sub(b.last).Seconds()*b.rate)
	}
	b.last = now
	if b.tokens < float64(n) {
		return false
	}
	b.tokens -= float64(n)
	return true
}

// metered reports whether a frame counts against the limits. out is true
// for frames the machine sends.
func metered(frame []byte, out bool, gateway netip.Addr) bool {
	if len(frame) < 14+20 || binary.BigEndian.Uint16(frame[12:14]) != etherIPv4 {
		return false // ARP and the like
	}
	ip := frame[14:]
	ihl := int(ip[0]&0x0f) * 4
	peer := netip.AddrFrom4([4]byte(ip[16:20]))
	if !out {
		peer = netip.AddrFrom4([4]byte(ip[12:16]))
	}
	if peer == netip.AddrFrom4([4]byte{255, 255, 255, 255}) {
		return false // DHCP
	}
	fragment := binary.BigEndian.Uint16(ip[6:8]) & 0x1fff
	if peer != gateway || fragment != 0 || len(ip) < ihl+4 || (ip[9] != protoTCP && ip[9] != protoUDP) {
		return true
	}
	src, dst := binary.BigEndian.Uint16(ip[ihl:]), binary.BigEndian.Uint16(ip[ihl+2:])
	guestPort, peerPort := src, dst
	if !out {
		guestPort, peerPort = dst, src
	}
	// The gateway's DNS, and the remote desktop that reaches the machine
	// through it, are free.
	return peerPort != 53 && guestPort != remoteDesktopPort
}

type limits struct {
	UpMbps   float64 `json:"up_mbps"`
	DownMbps float64 `json:"down_mbps"`
}

// watchLimits applies the limits the app writes to path, reading it every
// few seconds; no file means no limit.
func watchLimits(ctx context.Context, path string, up, down *bucket) {
	var last *limits
	for {
		var l limits
		if b, err := os.ReadFile(path); err == nil {
			if err := json.Unmarshal(b, &l); err != nil {
				l = limits{}
			}
		}
		if last == nil || l != *last {
			up.set(l.UpMbps)
			down.set(l.DownMbps)
			if l.UpMbps > 0 || l.DownMbps > 0 {
				log.Printf("machine bandwidth: up to %.1f Mbit/s out, %.1f Mbit/s in (0 is no limit)", l.UpMbps, l.DownMbps)
			} else if last != nil {
				log.Printf("machine bandwidth: no limit")
			}
			last = &l
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(5 * time.Second):
		}
	}
}
