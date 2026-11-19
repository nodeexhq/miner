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

func TestDelayPaces(t *testing.T) {
	var b bucket
	if b.delay(1<<30, time.Now()) != 0 {
		t.Fatal("no limit made a frame wait")
	}
	b.set(8) // 1 MB/s, a 256 KiB burst
	now := time.Unix(1000, 0)
	b.delay(0, now)
	// A sender with no delay of its own, as the virtual network's TCP is:
	// each frame goes as soon as the one before it was let through.
	sent := 0
	start := now
	for sent < 3_000_000 {
		now = now.Add(b.delay(1500, now))
		sent += 1500
	}
	took := now.Sub(start).Seconds()
	if took < 2.6 || took > 3.0 { // 3 MB less the burst, at 1 MB/s
		t.Errorf("3 MB took %.2f s at 1 MB/s", took)
	}
}

func TestLimitedConn(t *testing.T) {
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	c := &filteredConn{Conn: a, guestNet: mustCIDR(subnet), home: &homeAddrs{}, gateway: netip.MustParseAddr(gatewayIP)}
	c.down.set(8) // 1 MB/s, a 256 KiB burst
	received := make(chan int, 4096)
	go func() {
		buf := make([]byte, 2048)
		for {
			n, err := b.Read(buf)
			if err != nil {
				return
			}
			received <- n
		}
	}()
	bulk := packet("93.184.215.14", 443, guestIP, 50000, protoTCP, 1500)
	desk := packet(gatewayIP, 40000, guestIP, 5900, protoTCP, 1500)
	start := time.Now()
	for i := 0; i < 500; i++ { // 750 kB: the burst and half a second more
		c.Write(bulk)
	}
	took := time.Since(start)
	if took < 350*time.Millisecond || took > 1500*time.Millisecond {
		t.Errorf("750 kB at 1 MB/s took %v", took)
	}
	if got := len(received); got != 500 {
		t.Errorf("%d of 500 frames arrived: pacing must not lose any", got)
	}
	// The remote desktop does not wait, even with the bucket in debt.
	start = time.Now()
	for i := 0; i < 100; i++ {
		c.Write(desk)
	}
	if took := time.Since(start); took > 100*time.Millisecond {
		t.Errorf("the remote desktop waited %v behind the limit", took)
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

func TestHeldConnDropsEverything(t *testing.T) {
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	c := &filteredConn{Conn: a, guestNet: mustCIDR(subnet), home: &homeAddrs{}, gateway: netip.MustParseAddr(gatewayIP)}
	c.held.Store(true)
	// Nothing reaches the machine while held: the write is swallowed, so a
	// reader on the other end never sees it.
	got := make(chan int, 1)
	go func() {
		buf := make([]byte, 2048)
		n, _ := b.Read(buf)
		got <- n
	}()
	c.Write(packet(gatewayIP, 40000, guestIP, 5900, protoTCP, 200))
	select {
	case <-got:
		t.Fatal("a frame reached the machine while its network was held")
	case <-time.After(50 * time.Millisecond):
	}
	// And nothing from the machine gets out: Read skips it and waits.
	go b.Write(packet(guestIP, 50000, "93.184.215.14", 443, protoTCP, 200))
	read := make(chan struct{})
	go func() {
		buf := make([]byte, 2048)
		c.Read(buf)
		close(read)
	}()
	select {
	case <-read:
		t.Fatal("a frame from the machine got out while held")
	case <-time.After(50 * time.Millisecond):
	}
	c.held.Store(false)
	go b.Write(packet(guestIP, 50000, "93.184.215.14", 443, protoTCP, 200))
	select {
	case <-read:
	case <-time.After(time.Second):
		t.Fatal("the network did not come back")
	}
}

func TestPaceWaitsBrieflyThenDrops(t *testing.T) {
	var b bucket
	b.set(8) // 1 MB/s
	now := time.Now()
	b.pace(1<<30, now, 0) // nothing taken: far over
	now = now.Add(time.Second) // the bucket fills
	// Empty the burst.
	if w, ok := b.pace(int(b.burst), now, upQueue); !ok || w != 0 {
		t.Fatalf("burst: wait %v ok %v", w, ok)
	}
	// The next frames wait, each a little longer, up to the queue's length.
	var waited, dropped int
	var longest time.Duration
	for range 100 {
		w, ok := b.pace(1500, now, upQueue)
		if !ok {
			dropped++
			continue
		}
		waited++
		longest = max(longest, w)
	}
	if waited == 0 || dropped == 0 {
		t.Fatalf("waited %d dropped %d: want some of each", waited, dropped)
	}
	if longest > upQueue {
		t.Errorf("a frame waited %v, longer than %v", longest, upQueue)
	}
	// 40 ms at 1 MB/s is 40 kB: about 26 frames of 1500 bytes.
	if waited < 20 || waited > 30 {
		t.Errorf("%d frames queued, want about 26", waited)
	}
	// A dropped frame takes nothing: once time passes there is room again.
	if _, ok := b.pace(1500, now.Add(time.Second), upQueue); !ok {
		t.Error("no room a second later")
	}
}
