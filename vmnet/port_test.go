package main

import (
	"context"
	"net"
	"testing"
	"time"
)

func TestWaitForPort(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	// Freed after half a second, as an old helper leaves.
	go func() { time.Sleep(500 * time.Millisecond); ln.Close() }()
	start := time.Now()
	if err := waitForPort(context.Background(), addr, 5*time.Second); err != nil {
		t.Fatalf("port never came free: %v", err)
	}
	if time.Since(start) < 400*time.Millisecond {
		t.Fatal("did not wait for the port")
	}
	held, _ := net.Listen("tcp", "127.0.0.1:0")
	defer held.Close()
	if err := waitForPort(context.Background(), held.Addr().String(), 600*time.Millisecond); err == nil {
		t.Fatal("a port that stays taken was reported free")
	}
}
