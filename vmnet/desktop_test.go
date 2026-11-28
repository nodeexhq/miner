package main

import (
	"bufio"
	"context"
	"net"
	"net/netip"
	"testing"
	"time"
)

func TestGuestDesktopIsFree(t *testing.T) {
	gw := netip.MustParseAddr(gatewayIP)
	toDesktop := packet(guestIP, 50000, gatewayIP, 5901, protoTCP, 1500)
	fromDesktop := packet(gatewayIP, 5901, guestIP, 50000, protoTCP, 1500)
	if !metered(toDesktop, true, gw) {
		t.Fatal("a closed gateway port was free")
	}
	desktopGatewayPort = 5901
	defer func() { desktopGatewayPort = 0 }()
	if metered(toDesktop, true, gw) || metered(fromDesktop, false, gw) {
		t.Error("the host's desktop through the gateway was metered")
	}
	if !metered(packet(guestIP, 50000, "93.184.215.14", 5901, protoTCP, 1500), true, gw) {
		t.Error("port 5901 on the internet was free")
	}
}

func TestServeGuestDesktop(t *testing.T) {
	// The host's desktop: echoes a line back.
	host, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer host.Close()
	go func() {
		for {
			c, err := host.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				line, _ := bufio.NewReader(c).ReadString('\n')
				c.Write([]byte("host: " + line))
			}()
		}
	}()
	// Stands in for the listener in the machine's network.
	gateway, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go serveGuestDesktop(ctx, gateway, host.Addr().String())
	c, err := net.Dial("tcp", gateway.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(5 * time.Second))
	c.Write([]byte("RFB 003.008\n"))
	got, err := bufio.NewReader(c).ReadString('\n')
	if err != nil || got != "host: RFB 003.008\n" {
		t.Fatalf("got %q, %v", got, err)
	}
	cancel()
	time.Sleep(100 * time.Millisecond)
	if _, err := net.DialTimeout("tcp", gateway.Addr().String(), time.Second); err == nil {
		t.Error("the listener outlived its context")
	}
}
