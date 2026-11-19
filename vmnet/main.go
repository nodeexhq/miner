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
	"fmt"
	"log"
	"net"
	"net/netip"
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
	// The app holds the machine's network while its lease is not confirmed
	// (SIGUSR1) and lets it go again (SIGUSR2). Registered first: before
	// this, SIGUSR1 would end the helper, which also leaves no network.
	holds := make(chan os.Signal, 8)
	signal.Notify(holds, syscall.SIGUSR1, syscall.SIGUSR2)
	mac := flag.String("mac", "", "the machine's MAC address")
	sshListen := flag.String("ssh-listen", "127.0.0.1:22022", "host address forwarded to the machine's SSH")
	vncListen := flag.String("vnc-listen", "127.0.0.1:25900", "host address forwarded to the machine's remote desktop")
	limitsFile := flag.String("limits", "", "file with the machine's bandwidth limits, written by the app")
	api := flag.String("api", "https://app.nodeex.com", "platform API, asked for the owner's public address")
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

	// The helper of the machine before this one may still hold the ports
	// for a moment (when the app restarts, it leaves only once it notices,
	// within a second). Wait for them rather than fail.
	for _, addr := range []string{*sshListen, *vncListen} {
		if err := waitForPort(ctx, addr, 15*time.Second); err != nil {
			log.Fatalf("%s: %v", addr, err)
		}
	}

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

	home := &homeAddrs{}
	go watchHome(ctx, *api, home)
	filtered := &filteredConn{Conn: conn, guestNet: mustCIDR(subnet), home: home, gateway: netip.MustParseAddr(gatewayIP)}
	go func() {
		for s := range holds {
			held := s == syscall.SIGUSR1
			if filtered.held.Swap(held) != held {
				log.Printf("network held: %v", held)
			}
		}
	}()
	if *limitsFile != "" {
		go watchLimits(ctx, *limitsFile, &filtered.up, &filtered.down)
	}
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
	if u, d := filtered.limitedUp.Load(), filtered.limitedDown.Load(); u+d > 0 {
		log.Printf("bandwidth limits: dropped %d frames out, paced %d in", u, d)
	}
}

// waitForPort returns once addr can be listened on, or after limit.
func waitForPort(ctx context.Context, addr string, limit time.Duration) error {
	deadline := time.Now().Add(limit)
	for {
		ln, err := net.Listen("tcp", addr)
		if err == nil {
			return ln.Close()
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("still in use after %s: %w", limit, err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
}
