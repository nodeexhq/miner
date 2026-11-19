package main

import (
	"context"
	"encoding/binary"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"
)

// frame builds an Ethernet frame holding an IPv4 packet.
func frame(dst string, proto byte, dstPort uint16, fragmentOffset uint16) []byte {
	f := make([]byte, 14+20+8)
	binary.BigEndian.PutUint16(f[12:], etherIPv4)
	ip := f[14:]
	ip[0] = 0x45
	binary.BigEndian.PutUint16(ip[6:], fragmentOffset)
	ip[9] = proto
	a := netip.MustParseAddr(dst).As4()
	copy(ip[16:20], a[:])
	binary.BigEndian.PutUint16(ip[22:], dstPort)
	return f
}

func TestPolicy(t *testing.T) {
	guest := netip.MustParsePrefix(subnet)
	cases := []struct {
		name string
		f    []byte
		want bool
	}{
		{"public web", frame("93.184.215.14", protoTCP, 443, 0), true},
		{"public ssh", frame("140.82.112.3", protoTCP, 22, 0), true},
		{"public udp", frame("1.1.1.1", protoUDP, 53, 0), true},
		{"gateway dns", frame(gatewayIP, protoUDP, 53, 0), true},
		{"dhcp broadcast", frame("255.255.255.255", protoUDP, 67, 0), true},
		{"other broadcast", frame("255.255.255.255", protoUDP, 9, 0), false},
		{"owner's lan 192.168", frame("192.168.1.10", protoTCP, 445, 0), false},
		{"owner's router", frame("192.168.0.1", protoTCP, 80, 0), false},
		{"owner's lan 10", frame("10.0.0.243", protoTCP, 22, 0), false},
		{"owner's lan 172.16", frame("172.20.1.1", protoTCP, 80, 0), false},
		{"172.32 is public", frame("172.32.0.1", protoTCP, 80, 0), true},
		{"overlay", frame("100.64.0.3", protoTCP, 7422, 0), false},
		{"loopback", frame("127.0.0.1", protoTCP, 8080, 0), false},
		{"link local", frame("169.254.169.254", protoTCP, 80, 0), false},
		{"multicast", frame("224.0.0.251", protoUDP, 5353, 0), false},
		{"smtp", frame("142.250.0.27", protoTCP, 25, 0), false},
		{"smtps", frame("142.250.0.27", protoTCP, 465, 0), false},
		{"submission", frame("142.250.0.27", protoTCP, 587, 0), false},
		{"udp to port 25 is not mail", frame("1.1.1.1", protoUDP, 25, 0), true},
		{"later fragment to a public host", frame("93.184.215.14", protoTCP, 25, 100), true},
		{"later fragment to the lan", frame("192.168.1.10", protoTCP, 80, 100), false},
		{"truncated", make([]byte, 10), false},
	}
	for _, c := range cases {
		if got := allowed(c.f, guest, nil); got != c.want {
			t.Errorf("%s: allowed = %v, want %v", c.name, got, c.want)
		}
	}
	arp := make([]byte, 42)
	binary.BigEndian.PutUint16(arp[12:], etherARP)
	if !allowed(arp, guest, nil) {
		t.Error("ARP refused")
	}
	v6 := make([]byte, 60)
	binary.BigEndian.PutUint16(v6[12:], 0x86dd)
	if allowed(v6, guest, nil) {
		t.Error("IPv6 allowed")
	}
	short := frame("93.184.215.14", protoTCP, 443, 0)[:14+20+2]
	if allowed(short, guest, nil) {
		t.Error("a TCP packet without its ports was allowed")
	}
}

func TestHomeAddressRefused(t *testing.T) {
	guest := netip.MustParsePrefix(subnet)
	home := &homeAddrs{}
	to := frame("203.0.113.7", protoTCP, 443, 0)
	if !allowed(to, guest, home.list()) {
		t.Fatal("a public address is refused before it is known as home")
	}
	home.add(netip.MustParseAddr("203.0.113.7"))
	if allowed(to, guest, home.list()) {
		t.Error("the owner's public address is allowed")
	}
	if !allowed(frame("203.0.113.8", protoTCP, 443, 0), guest, home.list()) {
		t.Error("a neighbouring public address is refused")
	}
	// The address changes: the old one stays refused for a while.
	for _, a := range []string{"198.51.100.1", "198.51.100.2", "198.51.100.3"} {
		home.add(netip.MustParseAddr(a))
	}
	if allowed(to, guest, home.list()) {
		t.Error("a recent home address stopped being refused")
	}
	if home.add(netip.MustParseAddr("198.51.100.3")) {
		t.Error("the same address counted as new")
	}
	home.add(netip.MustParseAddr("198.51.100.4"))
	if l := home.list(); len(l) != keepHome || l[0] != netip.MustParseAddr("198.51.100.4") {
		t.Errorf("list = %v", l)
	}
}

func TestLookupHome(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/ip" {
			http.NotFound(w, r)
			return
		}
		w.Write([]byte(`{"ip":"::ffff:203.0.113.7"}`))
	}))
	defer srv.Close()
	a, err := lookupHome(context.Background(), srv.URL+"/")
	if err != nil || a != netip.MustParseAddr("203.0.113.7") {
		t.Fatalf("got %v, %v", a, err)
	}
	v6 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"ip":"2001:db8::1"}`))
	}))
	defer v6.Close()
	if _, err := lookupHome(context.Background(), v6.URL); err == nil {
		t.Error("an IPv6 answer was taken")
	}
}
