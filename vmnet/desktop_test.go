package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"io"
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

func encodings(list ...int32) []byte {
	b := []byte{2, 0, 0, 0}
	binary.BigEndian.PutUint16(b[2:], uint16(len(list)))
	for _, e := range list {
		b = binary.BigEndian.AppendUint32(b, uint32(e))
	}
	return b
}

// handshake is what a viewer sends before its first message: version,
// security type, the answer to VNC authentication and ClientInit.
func handshake(version string) []byte {
	b := []byte(version)
	if version != "RFB 003.003\n" {
		b = append(b, 2)
	}
	b = append(b, bytes.Repeat([]byte{0xab}, 16)...)
	return append(b, 1)
}

func TestFilterAddsDesktopSize(t *testing.T) {
	key := []byte{4, 1, 0, 0, 0, 0, 0, 'a'}
	pointer := []byte{5, 1, 0, 10, 0, 20}
	request := []byte{3, 0, 0, 0, 0, 0, 7, 128, 4, 176}
	pixel := append([]byte{0, 0, 0, 0}, make([]byte, 16)...)
	cut := []byte{6, 0, 0, 0, 0, 0, 0, 2, 'h', 'i'}
	for _, version := range []string{"RFB 003.008\n", "RFB 003.003\n"} {
		var in, want bytes.Buffer
		in.Write(handshake(version))
		want.Write(handshake(version))
		for _, m := range [][]byte{pixel, request, key, pointer, cut} {
			in.Write(m)
			want.Write(m)
		}
		in.Write(encodings(0, -239))
		want.Write(encodings(0, -239, -223))
		in.Write(encodings(-223, 0))
		want.Write(encodings(-223, 0))
		in.Write(key)
		want.Write(key)
		var out bytes.Buffer
		if err := filterViewer(&out, &in); err != io.EOF {
			t.Fatalf("%q: %v", version, err)
		}
		if !bytes.Equal(out.Bytes(), want.Bytes()) {
			t.Errorf("%q: sent\n%v\nwant\n%v", version, out.Bytes(), want.Bytes())
		}
	}
}

func TestFilterStopsAtUnknownMessages(t *testing.T) {
	in := bytes.NewReader(append(handshake("RFB 003.008\n"), 99, 1, 2, 3))
	if err := filterViewer(io.Discard, in); err == nil || err == io.EOF {
		t.Fatalf("an unknown message was passed on: %v", err)
	}
}

func TestServeHostDesktop(t *testing.T) {
	// The host's desktop: records what it is sent.
	host, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer host.Close()
	got := make(chan []byte, 1)
	go func() {
		c, err := host.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		c.Write([]byte("RFB 003.008\n"))
		want := len(handshake("RFB 003.008\n")) + 4 + 8
		b := make([]byte, want)
		io.ReadFull(c, b)
		got <- b
	}()
	gateway, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go serveHostDesktop(ctx, gateway, host.Addr().String())
	c, err := net.Dial("tcp", gateway.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(5 * time.Second))
	greeting := make([]byte, 12)
	if _, err := io.ReadFull(c, greeting); err != nil || string(greeting) != "RFB 003.008\n" {
		t.Fatalf("greeting %q, %v", greeting, err)
	}
	c.Write(handshake("RFB 003.008\n"))
	c.Write(encodings(0))
	select {
	case b := <-got:
		if tail := b[len(b)-12:]; !bytes.Equal(tail, encodings(0, -223)) {
			t.Errorf("the server was sent %v", tail)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("nothing reached the server")
	}
	cancel()
	time.Sleep(100 * time.Millisecond)
	if _, err := net.DialTimeout("tcp", gateway.Addr().String(), time.Second); err == nil {
		t.Error("the listener outlived its context")
	}
}
