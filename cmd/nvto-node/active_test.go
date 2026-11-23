package main

import (
	"net"
	"testing"
	"time"
)

func TestOnlyConnectionsCarryingDataCount(t *testing.T) {
	var s leaseState
	s.set("l1", time.Now().Add(time.Hour))
	busy, _ := net.Pipe()
	dead, _ := net.Pipe()
	b, d := newActiveConn(busy), newActiveConn(dead)
	if !s.track("l1", b) || !s.track("l1", d) {
		t.Fatal("track refused")
	}
	if n, _ := s.snapshot(); n != 2 {
		t.Fatalf("two fresh connections: %d", n)
	}
	// One went silent long ago, as a client that vanished without closing.
	d.last.Store(time.Now().Add(-activeWithin - time.Minute).UnixNano())
	if n, _ := s.snapshot(); n != 1 {
		t.Fatalf("a silent connection still counts: %d", n)
	}
}
