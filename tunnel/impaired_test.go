//go:build throughput

// Throughput of the overlay when the UDP path between the two nodes is
// a mediocre link instead of loopback: limited rate, a few milliseconds
// of delay, some loss and some reordering. The loopback benchmark in
// throughput_test.go shows what the software can carry; this one shows
// how a single TCP flow inside the overlay copes with a lossy line.
// Run with:
//
//	go test -tags throughput -run Impaired -v ./tunnel/
//
// IMPAIR_RATE (Mbit/s), IMPAIR_DELAY, IMPAIR_LOSS (percent),
// IMPAIR_REORDER (percent), IMPAIR_QUEUE and IMPAIR_SECS override the
// link settings.
package tunnel

import (
	"container/heap"
	"context"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"tailscale.com/envknob"
	"tailscale.com/ipn/store/mem"
	"tailscale.com/net/netns"
	"tailscale.com/tailcfg"
	"tailscale.com/tsnet"
	"tailscale.com/tstest/integration"
	"tailscale.com/tstest/integration/testcontrol"
	"tailscale.com/types/logger"
	"tailscale.com/wgengine/netstack"

	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/transport/tcp"
)

// linkSpec is one direction of the emulated line.
type linkSpec struct {
	rate    float64       // bits per second at the bottleneck
	delay   time.Duration // one-way propagation delay
	jitter  time.Duration // extra random delay, kept in order
	loss    float64       // probability a packet is dropped
	reorder float64       // probability a packet is held back by one delay
	queue   time.Duration // bottleneck queue; packets beyond it are dropped
}

func envFloat(name string, def float64) float64 {
	if v, err := strconv.ParseFloat(os.Getenv(name), 64); err == nil {
		return v
	}
	return def
}

func envDuration(name string, def time.Duration) time.Duration {
	if v, err := time.ParseDuration(os.Getenv(name)); err == nil {
		return v
	}
	return def
}

// wifiLink is roughly the owner's 2.4 GHz link: about 50 Mbit/s, a 10 ms
// round trip, 1% loss and a little reordering.
func wifiLink() linkSpec {
	return linkSpec{
		rate:    envFloat("IMPAIR_RATE", 50) * 1e6,
		delay:   envDuration("IMPAIR_DELAY", 5*time.Millisecond),
		jitter:  2 * time.Millisecond,
		loss:    envFloat("IMPAIR_LOSS", 1) / 100,
		reorder: envFloat("IMPAIR_REORDER", 0.5) / 100,
		queue:   envDuration("IMPAIR_QUEUE", 30*time.Millisecond),
	}
}

type delayed struct {
	at  time.Time
	seq uint64
	b   []byte
}

type delayHeap []delayed

func (h delayHeap) Len() int { return len(h) }
func (h delayHeap) Less(i, j int) bool {
	if !h[i].at.Equal(h[j].at) {
		return h[i].at.Before(h[j].at)
	}
	return h[i].seq < h[j].seq
}
func (h delayHeap) Swap(i, j int) { h[i], h[j] = h[j], h[i] }
func (h *delayHeap) Push(x any)   { *h = append(*h, x.(delayed)) }
func (h *delayHeap) Pop() any {
	old := *h
	x := old[len(old)-1]
	*h = old[:len(old)-1]
	return x
}

// lane carries one direction: packets read on in from src are shaped
// and written from out to dst.
type lane struct {
	spec     linkSpec
	out      *net.UDPConn
	dst      *net.UDPAddr
	mu       sync.Mutex
	h        delayHeap
	seq      uint64
	wake     chan struct{}
	depart   time.Time // when the bottleneck finishes the last packet
	arrive   time.Time // latest scheduled in-order arrival
	rng      *rand.Rand
	sent     atomic.Int64
	dropped  atomic.Int64
	overflow atomic.Int64
}

func (l *lane) offer(b []byte) {
	now := time.Now()
	l.mu.Lock()
	defer l.mu.Unlock()
	l.sent.Add(1)
	start := now
	if l.depart.After(start) {
		start = l.depart
	}
	if start.Sub(now) > l.spec.queue {
		l.overflow.Add(1)
		return
	}
	if l.spec.rate > 0 {
		l.depart = start.Add(time.Duration(float64(len(b)*8) / l.spec.rate * float64(time.Second)))
	} else {
		l.depart = start
	}
	if l.rng.Float64() < l.spec.loss {
		l.dropped.Add(1)
		return
	}
	at := l.depart.Add(l.spec.delay)
	if l.spec.jitter > 0 {
		at = at.Add(time.Duration(l.rng.Int64N(int64(l.spec.jitter))))
	}
	// Wi-Fi mostly delivers in order; only the chosen few overtake.
	if at.Before(l.arrive) {
		at = l.arrive
	}
	l.arrive = at
	if l.rng.Float64() < l.spec.reorder {
		at = at.Add(l.spec.delay)
	}
	l.seq++
	heap.Push(&l.h, delayed{at: at, seq: l.seq, b: append([]byte(nil), b...)})
	select {
	case l.wake <- struct{}{}:
	default:
	}
}

func (l *lane) run(ctx context.Context) {
	t := time.NewTimer(time.Hour)
	for {
		l.mu.Lock()
		var next time.Duration = time.Hour
		for l.h.Len() > 0 {
			d := l.h[0]
			if wait := time.Until(d.at); wait > 0 {
				next = wait
				break
			}
			heap.Pop(&l.h)
			l.mu.Unlock()
			l.out.WriteToUDP(d.b, l.dst)
			l.mu.Lock()
		}
		l.mu.Unlock()
		t.Reset(next)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-l.wake:
		}
	}
}

// relay stands between two nodes. Each node is told the other lives at a
// relay socket: sockA stands for A, sockB for B. What B sends to sockA is
// shaped and passed to A from sockB, so A sees B at B's advertised
// address, and the other way round.
type relay struct {
	sockA, sockB *net.UDPConn
	toA, toB     *lane
}

func listenUDP(t *testing.T) *net.UDPConn {
	c, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

func freeUDPPort(t *testing.T) uint16 {
	c := listenUDP(t)
	p := c.LocalAddr().(*net.UDPAddr).Port
	c.Close()
	return uint16(p)
}

func startRelay(t *testing.T, ctx context.Context, realA, realB *net.UDPAddr, aToB, bToA linkSpec) *relay {
	r := &relay{sockA: listenUDP(t), sockB: listenUDP(t)}
	newLane := func(spec linkSpec, out *net.UDPConn, dst *net.UDPAddr, seed uint64) *lane {
		l := &lane{spec: spec, out: out, dst: dst, wake: make(chan struct{}, 1), rng: rand.New(rand.NewPCG(seed, seed))}
		go l.run(ctx)
		return l
	}
	r.toA = newLane(bToA, r.sockB, realA, 1)
	r.toB = newLane(aToB, r.sockA, realB, 2)
	// in is the socket standing for the receiver; only its peer may use it.
	pump := func(in *net.UDPConn, from *net.UDPAddr, l *lane) {
		buf := make([]byte, 65535)
		for {
			n, src, err := in.ReadFromUDP(buf)
			if err != nil {
				return
			}
			if src.Port != from.Port {
				continue
			}
			l.offer(buf[:n])
		}
	}
	go pump(r.sockA, realB, r.toA)
	go pump(r.sockB, realA, r.toB)
	return r
}

func (l *lane) stats() string {
	return fmt.Sprintf("%d pkts, %d lost, %d queue drops", l.sent.Load(), l.dropped.Load(), l.overflow.Load())
}

// startImpairedNet runs control and DERP without STUN, so the nodes have
// no way to learn their real UDP addresses and can only use the relay.
func startImpairedNet(t *testing.T) string {
	netns.SetEnabled(false)
	t.Cleanup(func() { netns.SetEnabled(true) })
	derpMap := integration.RunDERPAndSTUN(t, logger.Discard, "127.0.0.1")
	for _, reg := range derpMap.Regions {
		for _, n := range reg.Nodes {
			n.STUNPort = -1
		}
	}
	control := &testcontrol.Server{DERPMap: derpMap, DNSConfig: &tailcfg.DNSConfig{Proxied: true}, MagicDNSDomain: "tail-scale.ts.net"}
	control.HTTPTestServer = httptest.NewUnstartedServer(control)
	control.HTTPTestServer.Start()
	t.Cleanup(control.HTTPTestServer.Close)
	return control.HTTPTestServer.URL
}

func startNodeOnPort(t *testing.T, ctx context.Context, url, name string, port uint16) (*tsnet.Server, string) {
	dir := filepath.Join(t.TempDir(), name)
	os.MkdirAll(dir, 0o700)
	s := &tsnet.Server{Dir: dir, ControlURL: url, Hostname: name, Store: new(mem.Store), Ephemeral: true, Port: port}
	t.Cleanup(func() { s.Close() })
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	tuneNode(t, s)
	st, err := s.Up(ctx)
	if err != nil {
		t.Fatal(err)
	}
	return s, st.TailscaleIPs[0].String()
}

// awaitDirect waits until from reaches peer over UDP through the relay
// rather than through DERP, which would bypass the impairment.
func awaitDirect(t *testing.T, ctx context.Context, from *tsnet.Server, peer string) string {
	lc, err := from.LocalClient()
	if err != nil {
		t.Fatal(err)
	}
	ip := netip.MustParseAddr(peer)
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		res, err := lc.Ping(ctx, ip, tailcfg.PingDisco)
		if err == nil && res.Endpoint != "" {
			return res.Endpoint
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatal("no direct path through the relay")
	return ""
}

// flowRate sends for d on each of conns at once and returns the combined
// Mbit/s the receivers counted.
func flowRate(t *testing.T, conns []net.Conn, recv []*atomic.Int64, d time.Duration) float64 {
	var wg sync.WaitGroup
	start := time.Now()
	before := int64(0)
	for _, r := range recv {
		before += r.Load()
	}
	for _, c := range conns {
		wg.Add(1)
		go func() {
			defer wg.Done()
			buf := make([]byte, 64<<10)
			c.SetWriteDeadline(start.Add(d))
			for {
				if _, err := c.Write(buf); err != nil {
					return
				}
			}
		}()
	}
	time.Sleep(d)
	after := int64(0)
	for _, r := range recv {
		after += r.Load()
	}
	secs := time.Since(start).Seconds()
	wg.Wait()
	return float64(after-before) * 8 / secs / 1e6
}

// counter accepts connections on ln and counts what they carry.
func counter(ln net.Listener) func() *atomic.Int64 {
	ch := make(chan *atomic.Int64, 16)
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			n := new(atomic.Int64)
			ch <- n
			go func() {
				buf := make([]byte, 64<<10)
				for {
					m, err := c.Read(buf)
					n.Add(int64(m))
					if err != nil {
						if err != io.EOF {
							c.Close()
						}
						return
					}
				}
			}()
		}
	}()
	return func() *atomic.Int64 { return <-ch }
}

func TestImpaired(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	secs := envDuration("IMPAIR_SECS", 10*time.Second)

	portA, portB := freeUDPPort(t), freeUDPPort(t)
	realA := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: int(portA)}
	realB := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: int(portB)}
	spec := wifiLink()
	r := startRelay(t, ctx, realA, realB, spec, spec)

	// Both nodes share this process and so these knobs; each relay socket
	// only forwards from the one node it serves, so a node that tries its
	// own stand-in gets nowhere and settles on the other.
	envknob.Setenv("TS_DEBUG_OMIT_LOCAL_ADDRS", "true")
	envknob.Setenv("TS_DEBUG_PRETENDPOINT", r.sockA.LocalAddr().String()+","+r.sockB.LocalAddr().String())
	t.Cleanup(func() {
		envknob.Setenv("TS_DEBUG_OMIT_LOCAL_ADDRS", "")
	})

	url := startImpairedNet(t)
	a, aIP := startNodeOnPort(t, ctx, url, "connect", portA)
	b, bIP := startNodeOnPort(t, ctx, url, "miner", portB)
	t.Logf("link each way: %.0f Mbit/s, %v delay, %.1f%% loss, %.1f%% reordered",
		spec.rate/1e6, spec.delay, spec.loss*100, spec.reorder*100)
	// The path must be the relay's stand-in for the peer, never DERP.
	if ep := awaitDirect(t, ctx, a, bIP); ep != r.sockB.LocalAddr().String() {
		t.Fatalf("connect reaches miner at %s, not through the relay", ep)
	}
	if ep := awaitDirect(t, ctx, b, aIP); ep != r.sockA.LocalAddr().String() {
		t.Fatalf("miner reaches connect at %s, not through the relay", ep)
	}

	run := func(label string, from *tsnet.Server, toIP string, to *tsnet.Server, port, flows int) {
		ln, err := to.Listen("tcp", fmt.Sprintf(":%d", port))
		if err != nil {
			t.Fatal(err)
		}
		defer ln.Close()
		next := counter(ln)
		var conns []net.Conn
		var recv []*atomic.Int64
		for range flows {
			c, err := from.Dial(ctx, "tcp", fmt.Sprintf("%s:%d", toIP, port))
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			conns = append(conns, c)
			recv = append(recv, next())
		}
		lost, queued := r.toA.dropped.Load()+r.toB.dropped.Load(), r.toA.overflow.Load()+r.toB.overflow.Load()
		rate := flowRate(t, conns, recv, secs)
		t.Logf("%-28s %6.1f Mbit/s  (relay: %d lost, %d queue drops)", label, rate,
			r.toA.dropped.Load()+r.toB.dropped.Load()-lost, r.toA.overflow.Load()+r.toB.overflow.Load()-queued)
	}
	run("miner -> connect, 1 flow", b, aIP, a, 7100, 1)
	run("connect -> miner, 1 flow", a, bIP, b, 7101, 1)
	run("miner -> connect, 4 flows", b, aIP, a, 7102, 4)
	t.Logf("relay to connect: %s", r.toA.stats())
	t.Logf("relay to miner:   %s", r.toB.stats())
}

// tuneNode applies netstack settings under trial on top of tsnet's own,
// before the node makes any connection: TUNE_CC (reno or cubic),
// TUNE_RACK (on or off), TUNE_RCVBUF and TUNE_SNDBUF (MiB) and
// TUNE_MINRTO. With none set the node runs exactly as the Miner and
// Connect do.
func tuneNode(t *testing.T, s *tsnet.Server) {
	ns, ok := s.Sys().Netstack.Get().(*netstack.Impl)
	if !ok {
		t.Fatal("no netstack")
	}
	set := func(what string, opt tcpip.SettableTransportProtocolOption) {
		if err := ns.SetTransportProtocolOption(tcp.ProtocolNumber, opt); err != nil {
			t.Fatalf("%s: %v", what, err)
		}
	}
	if v := os.Getenv("TUNE_CC"); v != "" {
		cc := tcpip.CongestionControlOption(v)
		set("congestion control", &cc)
	}
	switch os.Getenv("TUNE_RACK") {
	case "on":
		r := tcpip.TCPRACKLossDetection
		set("rack", &r)
	case "off":
		r := tcpip.TCPRecovery(0)
		set("rack", &r)
	}
	if mb := envFloat("TUNE_RCVBUF", 0); mb > 0 {
		n := int(mb * (1 << 20))
		set("rcvbuf", &tcpip.TCPReceiveBufferSizeRangeOption{Min: tcp.MinBufferSize, Default: n, Max: 4 * n})
	}
	if mb := envFloat("TUNE_SNDBUF", 0); mb > 0 {
		n := int(mb * (1 << 20))
		set("sndbuf", &tcpip.TCPSendBufferSizeRangeOption{Min: tcp.MinBufferSize, Default: n, Max: 4 * n})
	}
	if d := envDuration("TUNE_MINRTO", 0); d > 0 {
		o := tcpip.TCPMinRTOOption(d)
		set("minrto", &o)
	}
}
