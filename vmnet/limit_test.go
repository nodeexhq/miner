package main

import (
	"context"
	"encoding/binary"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// packet builds an Ethernet frame with an IPv4 TCP or UDP header.
func packet(src string, srcPort uint16, dst string, dstPort uint16, proto byte, size int) []byte {
	f := make([]byte, max(size, 14+20+8))
	binary.BigEndian.PutUint16(f[12:], etherIPv4)
	ip := f[14:]
	ip[0] = 0x45
	ip[9] = proto
	s, d := netip.MustParseAddr(src).As4(), netip.MustParseAddr(dst).As4()
	copy(ip[12:16], s[:])
	copy(ip[16:20], d[:])
	binary.BigEndian.PutUint16(ip[20:], srcPort)
	binary.BigEndian.PutUint16(ip[22:], dstPort)
	return f
}

func TestBucket(t *testing.T) {
	var b bucket
	if !b.allow(1<<30, time.Now()) {
		t.Fatal("no limit refused a frame")
	}
	b.set(8) // 1 MB/s, so a 256 KiB burst
	now := time.Unix(1000, 0)
	sent := 0
	for i := 0; i < 2000; i++ { // two seconds, a 1500-byte frame per millisecond
		now = now.Add(time.Millisecond)
		if b.allow(1500, now) {
			sent += 1500
		}
	}
	if sent < 1_900_000 || sent > 2_000_000+256<<10 {
		t.Errorf("sent %d bytes in two seconds at 1 MB/s", sent)
	}
	b.set(0)
	if !b.allow(1<<30, now) {
		t.Error("lifting the limit did not")
	}
}

func TestMetered(t *testing.T) {
	gw := netip.MustParseAddr(gatewayIP)
	cases := []struct {
		name string
		f    []byte
		out  bool
		want bool
	}{
		{"upload to the internet", packet(guestIP, 50000, "93.184.215.14", 443, protoTCP, 1500), true, true},
		{"download from the internet", packet("93.184.215.14", 443, guestIP, 50000, protoTCP, 1500), false, true},
		{"remote desktop out", packet(guestIP, 5900, gatewayIP, 40000, protoTCP, 1500), true, false},
		{"remote desktop in", packet(gatewayIP, 40000, guestIP, 5900, protoTCP, 200), false, false},
		{"gateway DNS", packet(guestIP, 53000, gatewayIP, 53, protoUDP, 80), true, false},
		{"DNS answer", packet(gatewayIP, 53, guestIP, 53000, protoUDP, 200), false, false},
		{"SSH through the tunnel counts", packet(guestIP, 22, gatewayIP, 40001, protoTCP, 1500), true, true},
		{"port 5900 on the internet counts", packet(guestIP, 50000, "93.184.215.14", 5900, protoTCP, 1500), true, true},
		{"the machine's 5900 to the internet counts", packet(guestIP, 5900, "93.184.215.14", 50000, protoTCP, 1500), true, true},
	}
	for _, c := range cases {
		if got := metered(c.f, c.out, gw); got != c.want {
			t.Errorf("%s: metered = %v", c.name, got)
		}
	}
}

func TestLimitedConn(t *testing.T) {
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	c := &filteredConn{Conn: a, guestNet: mustCIDR(subnet), home: &homeAddrs{}, gateway: netip.MustParseAddr(gatewayIP)}
	c.down.set(8)
	go func() {
		buf := make([]byte, 2048)
		for {
			if _, err := b.Read(buf); err != nil {
				return
			}
		}
	}()
	bulk := packet("93.184.215.14", 443, guestIP, 50000, protoTCP, 1500)
	desk := packet(gatewayIP, 40000, guestIP, 5900, protoTCP, 1500)
	for i := 0; i < 1000; i++ { // 1.5 MB at once: far past the burst
		c.Write(bulk)
	}
	if c.limitedDown.Load() == 0 {
		t.Fatal("nothing was held to the limit")
	}
	before := c.limitedDown.Load()
	for i := 0; i < 100; i++ {
		c.Write(desk)
	}
	if c.limitedDown.Load() != before {
		t.Error("the remote desktop was held back by the limit")
	}
}

func TestWatchLimits(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bandwidth.json")
	os.WriteFile(path, []byte(`{"up_mbps":16,"down_mbps":0}`), 0o600)
	var up, down bucket
	ctx, cancel := contextWithCancel()
	defer cancel()
	go watchLimits(ctx, path, &up, &down)
	deadline := time.Now().Add(2 * time.Second)
	for {
		up.mu.Lock()
		r := up.rate
		up.mu.Unlock()
		if r == 2e6 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("up rate = %v", r)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !down.allow(1<<30, time.Now()) {
		t.Error("a zero limit limited")
	}
}

func contextWithCancel() (context.Context, context.CancelFunc) {
	return context.WithCancel(context.Background())
}
