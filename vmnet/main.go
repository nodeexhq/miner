// nodeex-vmnet is the network of one virtual machine. The macOS app hands
// it the machine's network device as a datagram socket on standard input;
// every Ethernet frame the machine sends passes through here, in user space
// and without root, before anything reaches the owner's network.
//
// It gives the machine an address, DNS and a way out to the internet, and
// forwards two loopback ports of the host to the machine's SSH and remote
// desktop. What it refuses is in policy.go: a rented machine cannot reach
// the owner's local network or this Mac, and cannot send mail.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"log"
	"net"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/containers/gvisor-tap-vsock/pkg/types"
	"github.com/containers/gvisor-tap-vsock/pkg/virtualnetwork"
)

const (
	subnet    = "192.168.127.0/24"
	gatewayIP = "192.168.127.1"
	guestIP   = "192.168.127.2"
)

func main() {
	mac := flag.String("mac", "", "the machine's MAC address")
	sshListen := flag.String("ssh-listen", "127.0.0.1:22022", "host address forwarded to the machine's SSH")
	vncListen := flag.String("vnc-listen", "127.0.0.1:25900", "host address forwarded to the machine's remote desktop")
	flag.Parse()
	hw, err := net.ParseMAC(*mac)
	if err != nil {
		log.Fatalf("-mac: %v", err)
	}

	conn, err := net.FileConn(os.Stdin)
	if err != nil {
		log.Fatalf("standard input is not a socket: %v", err)
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	vn, err := virtualnetwork.New(&types.Configuration{
		MTU:               1500,
		Subnet:            subnet,
		GatewayIP:         gatewayIP,
		GatewayMacAddress: "5a:94:ef:e4:0c:dd",
		DHCPStaticLeases:  map[string]string{guestIP: hw.String()},
		Forwards: map[string]string{
			*sshListen: guestIP + ":22",
			*vncListen: guestIP + ":5900",
		},
		// No address inside the network stands for this Mac: the machine
		// has no way to reach services listening on the host.
		NAT:      map[string]string{},
		Protocol: types.VfkitProtocol,
	})
	if err != nil {
		log.Fatal(err)
	}
	json.NewEncoder(os.Stdout).Encode(map[string]any{"ready": true, "guest": guestIP, "ssh": *sshListen, "vnc": *vncListen})

	// A datagram socket does not report that its other end is gone, so the
	// app's death is noticed by its absence: this process is then adopted
	// by launchd. A network with no machine on it must not linger.
	parent := os.Getppid()
	go func() {
		for range time.Tick(time.Second) {
			if os.Getppid() != parent {
				cancel()
				return
			}
		}
	}()

	filtered := &filteredConn{Conn: conn, guestNet: mustCIDR(subnet)}
	done := make(chan error, 1)
	go func() { done <- vn.AcceptVfkit(ctx, filtered) }()
	select {
	case err := <-done:
		// The machine is gone: its end of the socket was closed.
		if err != nil && ctx.Err() == nil {
			log.Printf("network stopped: %v", err)
		}
	case <-ctx.Done():
	}
	conn.Close()
	if n := filtered.dropped.Load(); n > 0 {
		log.Printf("refused %d frames from the machine", n)
	}
}
