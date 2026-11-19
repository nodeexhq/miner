//go:build throughput

// Throughput of the overlay on one machine, with no real network in the
// way: two tsnet nodes joined through Tailscale's test control server.
// It separates what the software can carry from what a line or a relay
// allows. Run with:
//
//	go test -tags throughput -run Throughput -v ./tunnel/
package tunnel

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"tailscale.com/ipn/store/mem"
	"tailscale.com/net/netns"
	"tailscale.com/tailcfg"
	"tailscale.com/tsnet"
	"tailscale.com/tstest/integration"
	"tailscale.com/tstest/integration/testcontrol"
	"tailscale.com/types/logger"
)

const payload = 512 << 20 // bytes sent per measurement

func startNet(t *testing.T) string {
	netns.SetEnabled(false)
	t.Cleanup(func() { netns.SetEnabled(true) })
	derpMap := integration.RunDERPAndSTUN(t, logger.Discard, "127.0.0.1")
	control := &testcontrol.Server{DERPMap: derpMap, DNSConfig: &tailcfg.DNSConfig{Proxied: true}, MagicDNSDomain: "tail-scale.ts.net"}
	control.HTTPTestServer = httptest.NewUnstartedServer(control)
	control.HTTPTestServer.Start()
	t.Cleanup(control.HTTPTestServer.Close)
	return control.HTTPTestServer.URL
}

func startNode(t *testing.T, ctx context.Context, url, name string) (*tsnet.Server, string) {
	dir := filepath.Join(t.TempDir(), name)
	os.MkdirAll(dir, 0o700)
	s := &tsnet.Server{Dir: dir, ControlURL: url, Hostname: name, Store: new(mem.Store), Ephemeral: true}
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

// measure sends payload bytes into w and waits for the reader to report
// the count, returning megabits per second.
func measure(t *testing.T, w io.WriteCloser, got <-chan int64) float64 {
	buf := make([]byte, 256<<10)
	start := time.Now()
	for sent := 0; sent < payload; sent += len(buf) {
		if _, err := w.Write(buf); err != nil {
			t.Fatal(err)
		}
	}
	w.Close()
	n := <-got
	secs := time.Since(start).Seconds()
	if n != payload {
		t.Fatalf("received %d of %d bytes", n, payload)
	}
	return float64(n) * 8 / secs / 1e6
}

func sink(ln net.Listener) <-chan int64 {
	got := make(chan int64, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			got <- -1
			return
		}
		n, _ := io.Copy(io.Discard, c)
		c.Close()
		got <- n
	}()
	return got
}

func TestThroughput(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	url := startNet(t)
	a, _ := startNode(t, ctx, url, "connect")
	b, bIP := startNode(t, ctx, url, "miner")

	// A: plain TCP over the overlay.
	ln, err := b.Listen("tcp", ":7000")
	if err != nil {
		t.Fatal(err)
	}
	got := sink(ln)
	c, err := a.Dial(ctx, "tcp", bIP+":7000")
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("A overlay TCP:            %6.0f Mbit/s", measure(t, c, got))

	// B: the renter's path: a local port on the renter's computer, the
	// overlay, the gate's listener, and a loopback hop to the service, with
	// the same Pipe both ends use.
	target, _ := net.Listen("tcp", "127.0.0.1:0")
	got = sink(target)
	gateLn, err := b.Listen("tcp", ":7001")
	if err != nil {
		t.Fatal(err)
	}
	go Serve(ctx, gateLn, func(ctx context.Context) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "tcp", target.Addr().String())
	}, func(string, ...any) {})
	local, _ := net.Listen("tcp", "127.0.0.1:0")
	go Serve(ctx, local, func(ctx context.Context) (net.Conn, error) {
		return a.Dial(ctx, "tcp", bIP+":7001")
	}, func(string, ...any) {})
	c2, err := net.Dial("tcp", local.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("B renter path (forwards): %6.0f Mbit/s", measure(t, c2, got))
	fmt.Fprintln(os.Stderr, "(A and B are direct; the line or relay in between is what real sessions add)")
}
