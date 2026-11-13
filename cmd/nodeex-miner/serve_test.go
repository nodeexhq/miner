package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"testing"
	"time"
)

// A flood of connections that never finish the handshake must not be able
// to occupy the gate without bound.
func TestGateBoundsPendingHandshakes(t *testing.T) {
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	id := identity{NodeID: "node-1", PlatformKey: base64.StdEncoding.EncodeToString(pub)}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go serveGate(ctx, ln, id, &leaseState{}, gateTargets("127.0.0.1:1", "127.0.0.1:1"))

	const flood = maxHandshakes + 12
	var conns []net.Conn
	for range flood {
		c, err := net.Dial("tcp", ln.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		conns = append(conns, c)
	}
	// Give the gate a moment to accept them all, then see which were hung up on.
	time.Sleep(300 * time.Millisecond)
	dropped := 0
	for _, c := range conns {
		c.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
		if _, err := c.Read(make([]byte, 1)); err != nil {
			if ne, ok := err.(net.Error); !ok || !ne.Timeout() {
				dropped++
			}
		}
	}
	if dropped != flood-maxHandshakes {
		t.Errorf("%d of %d idle connections were dropped, want %d", dropped, flood, flood-maxHandshakes)
	}
}

func TestLeaseStateEndGrace(t *testing.T) {
	s := &leaseState{}
	s.set("lease-1", time.Now().Add(-endGrace/2))
	// Just past its end time the lease is still served: a renewal may be
	// on its way.
	if s.current() != "lease-1" {
		t.Error("lease cut off inside the grace period")
	}
	s.set("lease-1", time.Now().Add(-endGrace-time.Second))
	if s.current() != "" {
		t.Error("lease still served after the grace period")
	}
	// When the platform says there is no lease, access ends at once.
	s.set("lease-2", time.Now().Add(time.Hour))
	s.set("", time.Time{})
	if s.current() != "" {
		t.Error("lease served after the platform ended it")
	}
}

func TestSuspendedMinerServesNothing(t *testing.T) {
	s := &leaseState{}
	s.set("lease-1", time.Now().Add(time.Hour))
	a, b := net.Pipe()
	defer b.Close()
	if !s.track("lease-1", a) {
		t.Fatal("connection refused for the current lease")
	}

	// Cut off from the platform: the lease is no longer served and its
	// connections are closed.
	if !s.suspend() {
		t.Fatal("suspend reported no change")
	}
	if s.suspend() {
		t.Error("suspending twice reported a change twice")
	}
	if s.current() != "" {
		t.Error("lease still served while suspended")
	}
	b.SetReadDeadline(time.Now().Add(time.Second))
	if _, err := b.Read(make([]byte, 1)); err == nil {
		t.Error("connection left open after suspension")
	}
	c, d := net.Pipe()
	defer c.Close()
	defer d.Close()
	if s.track("lease-1", c) {
		t.Error("new connection accepted while suspended")
	}

	// The platform is heard from again and confirms the same lease.
	s.set("lease-1", time.Now().Add(time.Hour))
	if s.current() != "lease-1" || !s.track("lease-1", c) {
		t.Error("lease not served after contact with the platform returned")
	}
}

func TestAnswered(t *testing.T) {
	cases := map[string]struct {
		err  error
		want bool
	}{
		"refused by the platform":        {&statusError{401, "invalid signature"}, true},
		"edge error page, platform down": {&statusError{530, "HTTP 530"}, true},
		"wrapped status error":           {fmt.Errorf("heartbeat: %w", &statusError{502, "HTTP 502"}), true},
		"could not connect":              {&net.OpError{Op: "dial", Err: errors.New("connection refused")}, false},
		"name did not resolve":           {&net.DNSError{Err: "no such host", Name: "app.nodeex.com"}, false},
		"timed out":                      {context.DeadlineExceeded, false},
		"certificate not trusted":        {errors.New("x509: certificate signed by unknown authority"), false},
	}
	for name, c := range cases {
		if got := answered(c.err); got != c.want {
			t.Errorf("%s: answered = %v, want %v", name, got, c.want)
		}
	}
}
