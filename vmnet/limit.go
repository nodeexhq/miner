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
// The two directions are held differently, because the two senders differ.
//
// What the machine sends is paced too, but only a little: a frame waits for
// room up to upQueue, and is dropped beyond that. Dropping at once, with no
// wait at all, cost a single copy over SSH more than half of the limit
// (measured: 16 Mbit/s under 41.6, with 3% of the machine's packets lost);
// a short wait lets its TCP settle at the limit instead. The wait is kept
// short because frames are read from the machine one at a time, so the
// remote desktop waits behind a bulk transfer for at most that long.
//
// What the machine receives is paced: the frame is delivered when the limit
// has room for it. The sender here is the virtual network's own TCP, right
// next to the machine with no delay in between; dropping its frames made it
// collapse (measured on a Mac mini: 9 Mbit/s under a 105 Mbit/s limit, 146
// with none). Frames are written to the machine one at a time, so waiting
// here holds the senders back directly, and a frame that does not count
// waits at most for the one ahead of it.

const remoteDesktopPort = 5900

// upQueue is the longest a frame the machine sends waits for room.
const upQueue = 40 * time.Millisecond

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

// delay takes n bytes from the bucket, going into debt if it must, and
// returns how long the caller should wait before sending them.
func (b *bucket) delay(n int, now time.Time) time.Duration {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.rate <= 0 {
		return 0
	}
	if !b.last.IsZero() {
		b.tokens = min(b.burst, b.tokens+now.Sub(b.last).Seconds()*b.rate)
	}
	b.last = now
	b.tokens -= float64(n)
	if b.tokens >= 0 {
		return 0
	}
	return time.Duration(-b.tokens / b.rate * float64(time.Second))
}

// pace takes n bytes from the bucket if they can leave within limit, and
// returns how long to wait before sending them; ok is false, and nothing is
// taken, when the wait would be longer.
func (b *bucket) pace(n int, now time.Time, limit time.Duration) (wait time.Duration, ok bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.rate <= 0 {
		return 0, true
	}
	if !b.last.IsZero() {
		b.tokens = min(b.burst, b.tokens+now.Sub(b.last).Seconds()*b.rate)
	}
	b.last = now
	after := b.tokens - float64(n)
	if after >= 0 {
		b.tokens = after
		return 0, true
	}
	wait = time.Duration(-after / b.rate * float64(time.Second))
	if wait > limit {
		return 0, false
	}
	b.tokens = after
	return wait, true
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
	// Optional: the abuse thresholds (abuse.go); absent is the defaults.
	Abuse abuseLimits `json:"abuse"`
}

// watchLimits applies the limits the app writes to path, reading it every
// few seconds; no file means no bandwidth limit and the default abuse
// thresholds. guard may be nil.
func watchLimits(ctx context.Context, path string, up, down *bucket, guard *abuseGuard) {
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
			if guard != nil && (last == nil || l.Abuse != last.Abuse) {
				guard.set(l.Abuse)
				if l.Abuse != (abuseLimits{}) {
					log.Printf("abuse limits: %+v", l.Abuse.effective())
				}
			}
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
