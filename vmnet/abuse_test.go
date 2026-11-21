package main

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// tcpFrame builds an Ethernet frame with an IPv4 TCP header and flags.
func tcpFrame(src string, sport uint16, dst string, dport uint16, flags byte) []byte {
	f := packet(src, sport, dst, dport, protoTCP, 14+20+20)
	f[14+20+12] = 5 << 4
	f[14+20+13] = flags
	return f
}

func udpFrame(src string, sport uint16, dst string, dport uint16, size int) []byte {
	return packet(src, sport, dst, dport, protoUDP, size)
}

func pingFrame(dst string) []byte {
	f := packet(guestIP, 0, dst, 0, protoICMP, 14+20+8)
	f[14+20] = icmpEcho
	return f
}

// host returns a distinct public address for each i.
func host(i int) string {
	return fmt.Sprintf("%d.%d.%d.%d", 20+i>>16&0x3f, byte(i>>8), byte(i), 7)
}

type testGuard struct {
	*abuseGuard
	t      *testing.T
	now    time.Time
	events []abuseEvent
	passed int
	denied int
}

func newTestGuard(t *testing.T, l abuseLimits) *testGuard {
	tg := &testGuard{t: t}
	tg.abuseGuard = newAbuseGuard(netip.MustParseAddr(gatewayIP).As4(), func(e abuseEvent) { tg.events = append(tg.events, e) })
	tg.set(l)
	tg.now = time.Now()
	return tg
}

func (g *testGuard) advance(d time.Duration) {
	g.now = g.now.Add(d)
	g.tick(g.now)
}

func (g *testGuard) send(f []byte) bool {
	ok := g.out(f, g.now)
	if ok {
		g.passed++
	} else {
		g.denied++
	}
	return ok
}

func (g *testGuard) receive(f []byte) { g.in(f, g.now) }

func (g *testGuard) wantClean() {
	g.t.Helper()
	if g.isTripped() {
		g.t.Fatalf("tripped: %+v", g.events)
	}
	if f, u := g.droppedFlows.Load(), g.droppedUDP.Load(); f+u > 0 {
		g.t.Fatalf("soft limits dropped %d flows, %d UDP packets", f, u)
	}
}

func (g *testGuard) wantTripped(reason string) {
	g.t.Helper()
	if !g.isTripped() || len(g.events) != 1 {
		g.t.Fatalf("not tripped once: %+v", g.events)
	}
	if g.events[0].Reason != reason || g.events[0].Event != "abuse" {
		g.t.Fatalf("tripped for %+v, want %s", g.events[0], reason)
	}
}

// --- Normal work passes.

func TestNormalConnectionsPass(t *testing.T) {
	g := newTestGuard(t, abuseLimits{})
	// 100 connections to 20 hosts over 10 s, each answered.
	for i := range 100 {
		dst, sport := host(i%20), uint16(50000+i)
		if !g.send(tcpFrame(guestIP, sport, dst, 443, tcpSYN)) {
			t.Fatalf("connection %d refused", i)
		}
		g.send(tcpFrame(guestIP, sport, dst, 443, tcpSYN)) // a retransmit
		g.receive(tcpFrame(dst, 443, guestIP, sport, tcpSYN|tcpACK))
		g.send(tcpFrame(guestIP, sport, dst, 443, tcpACK))
		g.advance(100 * time.Millisecond)
	}
	g.advance(time.Minute)
	g.wantClean()
}

func TestBrowserBurstPasses(t *testing.T) {
	g := newTestGuard(t, abuseLimits{})
	// A heavy page: 150 connections to 120 hosts in one instant, then
	// QUIC to 30 of them.
	for i := range 150 {
		if !g.send(tcpFrame(guestIP, uint16(40000+i), host(i%120), 443, tcpSYN)) {
			t.Fatalf("connection %d refused", i)
		}
		g.receive(tcpFrame(host(i%120), 443, guestIP, uint16(40000+i), tcpSYN|tcpACK))
	}
	for i := range 30 {
		g.send(udpFrame(guestIP, uint16(60000+i), host(i), 443, 1300))
	}
	g.advance(time.Minute)
	g.wantClean()
}

func TestUDPStreamPasses(t *testing.T) {
	g := newTestGuard(t, abuseLimits{})
	// 1 MB/s to one host in 1200-byte packets, with nothing heard back:
	// about 833 packets a second, for a minute.
	for range 60 * 833 {
		if !g.send(udpFrame(guestIP, 50000, "203.0.113.50", 3478, 1200)) {
			t.Fatal("a stream packet was dropped")
		}
		g.advance(time.Second / 833)
	}
	g.wantClean()
}

func TestRemoteDesktopRelayPasses(t *testing.T) {
	g := newTestGuard(t, abuseLimits{})
	// The machine's screen to a relay: 4500 packets a second (50 Mbit/s),
	// answered by the relay many times a second, for a minute.
	for i := range 60 * 4500 {
		if !g.send(udpFrame(guestIP, 50000, "203.0.113.60", 21116, 1400)) {
			t.Fatalf("relay packet %d dropped", i)
		}
		if i%50 == 0 {
			g.receive(udpFrame("203.0.113.60", 21116, guestIP, 50000, 100))
		}
		g.advance(time.Second / 4500)
	}
	g.wantClean()
}

func TestDNSBurstsPass(t *testing.T) {
	g := newTestGuard(t, abuseLimits{})
	for round := range 5 {
		// 1000 queries in an instant to the gateway's DNS...
		for i := range 1000 {
			if !g.send(udpFrame(guestIP, uint16(30000+i), gatewayIP, 53, 80)) {
				t.Fatal("gateway DNS dropped")
			}
		}
		// ...and 300 to a public resolver, each from its own port.
		for i := range 300 {
			if !g.send(udpFrame(guestIP, uint16(31000+i), "1.1.1.1", 53, 80)) {
				t.Fatalf("round %d: query %d to 1.1.1.1 dropped", round, i)
			}
			g.receive(udpFrame("1.1.1.1", 53, guestIP, uint16(31000+i), 200))
		}
		g.advance(10 * time.Second)
	}
	g.wantClean()
}

// --- Abuse is stopped.

func TestSYNTo1000Hosts(t *testing.T) {
	g := newTestGuard(t, abuseLimits{})
	// A scan of port 22 on 1000 hosts over 10 s; none answers.
	for i := range 1000 {
		g.send(tcpFrame(guestIP, uint16(10000+i), host(i), 22, tcpSYN))
		g.advance(10 * time.Millisecond)
	}
	if g.droppedFlows.Load() == 0 {
		t.Error("100 attempts a second were not slowed down")
	}
	g.advance(40 * time.Second) // the attempts time out
	g.wantTripped(reasonHalfOpen)
}

func TestSYNToManyRespondingHosts(t *testing.T) {
	g := newTestGuard(t, abuseLimits{})
	// A sweep of hosts that do answer, under the new-flow rate.
	for i := range 2000 {
		g.send(tcpFrame(guestIP, uint16(10000+i), host(i), 443, tcpSYN))
		g.receive(tcpFrame(host(i), 443, guestIP, uint16(10000+i), tcpSYN|tcpACK))
		g.advance(25 * time.Millisecond)
		if g.isTripped() {
			break
		}
	}
	g.wantTripped(reasonFanOut)
	if g.droppedFlows.Load() != 0 {
		t.Error("a sweep under the rate was slowed")
	}
}

func TestPortScan(t *testing.T) {
	g := newTestGuard(t, abuseLimits{})
	// 200 ports of one host in two seconds; closed ones refuse.
	for p := range 200 {
		ok := g.send(tcpFrame(guestIP, uint16(20000+p), "198.51.100.20", uint16(1+p), tcpSYN))
		if !ok {
			break
		}
		g.receive(tcpFrame("198.51.100.20", uint16(1+p), guestIP, uint16(20000+p), tcpRST|tcpACK))
		g.advance(10 * time.Millisecond)
	}
	g.wantTripped(reasonPortScan)
	if !strings.Contains(g.events[0].Detail, "198.51.100.20") {
		t.Errorf("detail %q does not name the host", g.events[0].Detail)
	}
}

func TestUDPFloodToManyHosts(t *testing.T) {
	g := newTestGuard(t, abuseLimits{})
	// 5000 small packets a second, each to another host, for up to 10 s.
	for i := range 50000 {
		g.send(udpFrame(guestIP, 40000, host(i), 53, 60))
		g.advance(time.Second / 5000)
		if g.isTripped() {
			break
		}
	}
	if g.droppedUDP.Load()+g.droppedFlows.Load() == 0 {
		t.Error("no soft drops before the cut")
	}
	g.wantTripped(reasonFanOut)
}

func TestUDPFloodToOneHost(t *testing.T) {
	g := newTestGuard(t, abuseLimits{})
	// 5000 packets a second to one silent host: dropped down to the quiet
	// rate at once, cut after half a minute of it.
	for range 40 * 5000 {
		g.send(udpFrame(guestIP, 40000, "198.51.100.30", 80, 60))
		g.advance(time.Second / 5000)
		if g.isTripped() {
			break
		}
	}
	g.wantTripped(reasonSustainedUDP)
	if g.passed > 33*2000+4000 {
		t.Errorf("%d packets passed, more than the quiet rate allows", g.passed)
	}
}

func TestUnansweredSYNFlood(t *testing.T) {
	g := newTestGuard(t, abuseLimits{})
	// 50 attempts a second at one silent host, under the new-flow rate.
	for i := range 60 * 50 {
		if !g.send(tcpFrame(guestIP, uint16(1024+i), "198.51.100.40", 80, tcpSYN)) {
			break
		}
		g.advance(20 * time.Millisecond)
	}
	g.wantTripped(reasonHalfOpen)
	if g.droppedFlows.Load() != 0 {
		t.Error("a flood under the rate was slowed rather than cut")
	}
}

func TestRefusedSYNFlood(t *testing.T) {
	g := newTestGuard(t, abuseLimits{})
	for i := range 1000 {
		if !g.send(tcpFrame(guestIP, uint16(1024+i), "198.51.100.41", 80, tcpSYN)) {
			break
		}
		g.receive(tcpFrame("198.51.100.41", 80, guestIP, uint16(1024+i), tcpRST|tcpACK))
		g.advance(20 * time.Millisecond)
	}
	g.wantTripped(reasonHalfOpen)
}

func TestSustainedNewFlows(t *testing.T) {
	g := newTestGuard(t, abuseLimits{})
	start := g.now
	// 200 answered connections a second to five hosts, without pause.
	for i := range 40 * 200 {
		sport := uint16(1024 + i%60000)
		if g.send(tcpFrame(guestIP, sport, host(i%5), 443, tcpSYN)) {
			g.receive(tcpFrame(host(i%5), 443, guestIP, sport, tcpSYN|tcpACK))
		}
		g.advance(5 * time.Millisecond)
		if g.isTripped() {
			break
		}
	}
	g.wantTripped(reasonSustainedNew)
	if took := g.now.Sub(start); took < 30*time.Second || took > 35*time.Second {
		t.Errorf("cut after %v, want about 30 s", took)
	}
}

func TestShortOverrunIsNotSustained(t *testing.T) {
	g := newTestGuard(t, abuseLimits{})
	// Bursts over the rate, with pauses between: soft drops only.
	for burst := range 10 {
		for i := range 400 {
			sport := uint16(1024 + burst*400 + i)
			if g.send(tcpFrame(guestIP, sport, host(i%10), 443, tcpSYN)) {
				g.receive(tcpFrame(host(i%10), 443, guestIP, sport, tcpSYN|tcpACK))
			}
		}
		g.advance(10 * time.Second)
	}
	if g.isTripped() {
		t.Fatalf("tripped: %+v", g.events)
	}
	if g.droppedFlows.Load() == 0 {
		t.Error("bursts over the rate were not slowed")
	}
}

func TestPingSweep(t *testing.T) {
	g := newTestGuard(t, abuseLimits{})
	for i := range 3000 {
		g.send(pingFrame(host(i)))
		g.advance(20 * time.Millisecond)
		if g.isTripped() {
			break
		}
	}
	g.wantTripped(reasonFanOut)
}

// --- The cut.

func TestAbuseCutIsSticky(t *testing.T) {
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	gw := netip.MustParseAddr(gatewayIP)
	// Events as main writes them, a JSON line each.
	events := make(chan []byte, 4)
	guard := newAbuseGuard(gw.As4(), func(e abuseEvent) {
		var line bytes.Buffer
		(&jsonLines{w: &line}).write(e)
		events <- line.Bytes()
	})
	c := &filteredConn{Conn: a, guestNet: mustCIDR(subnet), home: &homeAddrs{}, gateway: gw, abuse: guard}

	// Received frames reach the machine before the cut.
	got := make(chan int, 16)
	go func() {
		buf := make([]byte, 2048)
		for {
			n, err := b.Read(buf)
			if err != nil {
				return
			}
			got <- n
		}
	}()
	c.Write(packet("93.184.215.14", 443, guestIP, 50000, protoTCP, 200))
	select {
	case <-got:
	case <-time.After(time.Second):
		t.Fatal("the network does not carry frames before a cut")
	}

	// A port scan through the connection itself, the virtual network
	// reading what the machine sends.
	sent := make(chan struct{}, 256)
	go func() {
		buf := make([]byte, 2048)
		for {
			if _, err := c.Read(buf); err != nil {
				return
			}
			sent <- struct{}{}
		}
	}()
	for p := range 200 {
		b.Write(tcpFrame(guestIP, uint16(20000+p), "198.51.100.20", uint16(1+p), tcpSYN))
	}
	var e map[string]string
	select {
	case line := <-events:
		if err := json.Unmarshal(line, &e); err != nil {
			t.Fatalf("event %q: %v", line, err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the scan was not caught")
	}
	if len(sent) != 100 {
		t.Errorf("%d attempts got out, want the first 100 ports", len(sent))
	}
	// The event, as the app reads it.
	if e["event"] != "abuse" || e["reason"] != reasonPortScan || e["detail"] == "" || e["time"] == "" {
		t.Errorf("event %v", e)
	}

	// The app's lease hold and release, as SIGUSR1 and SIGUSR2 do.
	c.leaseHold(true)
	c.leaseHold(false)
	if c.held.Load() {
		t.Fatal("release did not release the lease hold")
	}
	if !c.cut() {
		t.Fatal("a release lifted the cut for abuse")
	}
	c.Write(packet("93.184.215.14", 443, guestIP, 50000, protoTCP, 200))
	select {
	case <-got:
		t.Fatal("a frame reached the machine after the cut")
	case <-time.After(50 * time.Millisecond):
	}
	for len(sent) > 0 {
		<-sent
	}
	b.Write(packet(guestIP, 50000, "93.184.215.14", 443, protoTCP, 200))
	b.Write(udpFrame(guestIP, 50000, "93.184.215.14", 443, 200))
	select {
	case <-sent:
		t.Fatal("a frame from the machine got out after the cut")
	case <-time.After(50 * time.Millisecond):
	}
	// Only one event, however much more the machine tries.
	select {
	case line := <-events:
		t.Errorf("a second event: %s", line)
	default:
	}
}

func TestLeaseHoldWithoutAbuse(t *testing.T) {
	c := &filteredConn{abuse: newAbuseGuard(netip.MustParseAddr(gatewayIP).As4(), nil)}
	if !c.leaseHold(true) || !c.cut() {
		t.Fatal("hold did not hold")
	}
	if c.leaseHold(true) {
		t.Error("a second hold counted as a change")
	}
	if !c.leaseHold(false) || c.cut() {
		t.Fatal("release did not release")
	}
}

// --- Memory.

func TestTablesStayBounded(t *testing.T) {
	// The patterns off, so nothing stops the flood: only the caps hold.
	g := newTestGuard(t, abuseLimits{
		NewFlowsPerSec: 1e9, NewFlowsBurst: 1e9, UDPPerSec: 1e9, UDPBurst: 1e9, UDPAllPerSec: 1e9, UDPAllBurst: 1e9,
		DistinctIPs: -1, PortsPerIP: -1, UnansweredSYNs: -1, SustainedSec: -1,
	})
	for i := range 200_000 {
		g.send(udpFrame(guestIP, 40000, host(i), uint16(i), 60))
		g.send(tcpFrame(guestIP, uint16(i), host(i+7), uint16(i>>3), tcpSYN))
		g.advance(time.Microsecond)
	}
	g.mu.Lock()
	ips, pairs, ports, pending := len(g.ips), len(g.pairs), len(g.portsOf), len(g.pending)
	g.mu.Unlock()
	if ips > maxIPs || pairs > maxPairs || ports > maxPairs || pending > maxPending {
		t.Errorf("tables grew: %d hosts, %d flows, %d port counts, %d attempts", ips, pairs, ports, pending)
	}
	if g.isTripped() {
		t.Error("tripped with every pattern off")
	}
	// Everything ages out.
	g.advance(2 * time.Minute)
	g.mu.Lock()
	defer g.mu.Unlock()
	if len(g.ips)+len(g.pairs)+len(g.portsOf)+len(g.pending) != 0 {
		t.Errorf("left after the window: %d %d %d %d", len(g.ips), len(g.pairs), len(g.portsOf), len(g.pending))
	}
}

func TestTablesBoundedWithDefaults(t *testing.T) {
	g := newTestGuard(t, abuseLimits{})
	for i := range 100_000 {
		g.send(udpFrame(guestIP, 40000, host(i), 9, 60))
		g.advance(time.Microsecond)
	}
	if !g.isTripped() {
		t.Fatal("a flood of unique destinations was not cut")
	}
	if len(g.ips) > maxIPs || len(g.pairs) > maxPairs {
		t.Errorf("%d hosts, %d flows", len(g.ips), len(g.pairs))
	}
}

// --- Configuration.

func TestAbuseLimitsFromFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bandwidth.json")
	os.WriteFile(path, []byte(`{"up_mbps":16,"down_mbps":0,"abuse":{"ports_per_ip_per_min":20,"distinct_ips_per_min":-1,"new_flows_per_sec":5}}`), 0o600)
	guard := newAbuseGuard(netip.MustParseAddr(gatewayIP).As4(), nil)
	var up, down bucket
	ctx, cancel := contextWithCancel()
	defer cancel()
	go watchLimits(ctx, path, &up, &down, guard)
	deadline := time.Now().Add(2 * time.Second)
	for {
		guard.mu.Lock()
		cfg := guard.cfg
		guard.mu.Unlock()
		if cfg.PortsPerIP == 20 {
			if cfg.DistinctIPs != 0 || cfg.NewFlowsPerSec != 5 || cfg.NewFlowsBurst != defaultAbuse.NewFlowsBurst || cfg.UnansweredSYNs != defaultAbuse.UnansweredSYNs {
				t.Errorf("limits %+v", cfg)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("limits not applied: %+v", cfg)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestAbuseLimitsDefaultsAndOff(t *testing.T) {
	var l limits
	json.Unmarshal([]byte(`{"up_mbps":10}`), &l)
	if l.Abuse.effective() != defaultAbuse {
		t.Errorf("no abuse object: %+v", l.Abuse.effective())
	}
	if e := (abuseLimits{DistinctIPs: 1 << 30}).effective(); e.DistinctIPs >= maxIPs {
		t.Errorf("a threshold above the table size: %d", e.DistinctIPs)
	}
	g := newTestGuard(t, abuseLimits{Off: true})
	for p := range 500 {
		if !g.send(tcpFrame(guestIP, uint16(20000+p), "198.51.100.20", uint16(1+p), tcpSYN)) {
			t.Fatal("dropped with the checks off")
		}
	}
	g.wantClean()
}

func TestParseIgnoresOtherFrames(t *testing.T) {
	g := newTestGuard(t, abuseLimits{})
	arp := make([]byte, 42)
	binary.BigEndian.PutUint16(arp[12:], etherARP)
	if !g.send(arp) || !g.send(make([]byte, 20)) {
		t.Error("a frame that is not IPv4 was dropped")
	}
	// TCP that is not an attempt takes no part.
	for i := range 10000 {
		if !g.send(tcpFrame(guestIP, uint16(i), host(i), uint16(i), tcpACK)) {
			t.Fatal("TCP data dropped")
		}
	}
	g.wantClean()
}
