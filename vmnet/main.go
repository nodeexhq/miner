// nvto-vmnet is the network of one virtual machine. The macOS app hands
// it the machine's network device as a datagram socket on standard input;
// every Ethernet frame the machine sends passes through here, in user space
// and without root, before anything reaches the owner's network.
//
// It gives the machine an address, DNS and a way out to the internet, and
// forwards two loopback ports of the host to the machine's SSH and remote
// desktop. What it refuses is in policy.go: a rented machine cannot reach
// the owner's local network or this Mac, and cannot send mail; abuse.go
// stops it scanning or flooding from the owner's address.
package main

import (
	"context"
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
	hostDesktop := flag.String("host-desktop", "", "the host's own VNC server for the machine; -vnc-listen then leads there instead of to the machine's")
	guestDesktopPort := flag.Int("guest-desktop-port", 0, "port on the gateway where the machine reaches -host-desktop; 0 for none")
	limitsFile := flag.String("limits", "", "file with the machine's bandwidth limits, written by the app")
	api := flag.String("api", "https://app.nvto.com", "platform API, asked for the owner's public address")
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
	// With the host serving the remote desktop (Virtualization's own VNC
	// server, see HostDesktop in the app) the remote desktop port leads to
	// it, through the filter in rfb.go, not to the machine's.
	forwards := map[string]string{*sshListen: guestIP + ":22"}
	if *hostDesktop == "" {
		forwards[*vncListen] = guestIP + ":5900"
	} else if *guestDesktopPort < 0 || *guestDesktopPort > 65535 {
		log.Fatalf("-guest-desktop-port %d", *guestDesktopPort)
	}
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
		Forwards:          forwards,
		// No address inside the network stands for this Mac: the machine
		// has no way to reach services listening on the host.
		NAT:      map[string]string{},
		Protocol: types.VfkitProtocol,
	})
	if err != nil {
		log.Fatal(err)
	}
	if *hostDesktop != "" {
		ln, err := net.Listen("tcp", *vncListen)
		if err != nil {
			log.Fatalf("-vnc-listen: %v", err)
		}
		go serveHostDesktop(ctx, ln, *hostDesktop)
	}
	if *hostDesktop != "" && *guestDesktopPort > 0 {
		// The machine itself may reach the host's remote desktop, at one
		// port of its gateway and nowhere else on the host: a desktop in a
		// browser is carried to it inside the renter's SSH, end to end,
		// because the platform relays the browser's traffic and RFB is not
		// encrypted. The listener lives in this network's own stack, so
		// only the machine can connect to it.
		ln, err := vn.Listen("tcp", fmt.Sprintf("%s:%d", gatewayIP, *guestDesktopPort))
		if err != nil {
			log.Fatalf("-guest-desktop-port: %v", err)
		}
		desktopGatewayPort = uint16(*guestDesktopPort)
		go serveHostDesktop(ctx, ln, *hostDesktop)
	}
	// Standard output carries JSON lines for the app: this one, and one
	// {"event":"abuse",...} if the network is cut for abuse.
	stdout := &jsonLines{w: os.Stdout}
	stdout.write(map[string]any{"ready": true, "guest": guestIP, "ssh": *sshListen, "vnc": *vncListen})

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
	gw := netip.MustParseAddr(gatewayIP)
	guard := newAbuseGuard(gw.As4(), func(e abuseEvent) { stdout.write(e) })
	filtered := &filteredConn{Conn: conn, guestNet: mustCIDR(subnet), home: home, gateway: gw, abuse: guard}
	go func() {
		t := time.NewTicker(time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case now := <-t.C:
				guard.tick(now)
			}
		}
	}()
	go func() {
		for s := range holds {
			filtered.leaseHold(s == syscall.SIGUSR1)
		}
	}()
	if *limitsFile != "" {
		go watchLimits(ctx, *limitsFile, &filtered.up, &filtered.down, guard)
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
	if f, u := guard.droppedFlows.Load(), guard.droppedUDP.Load(); f+u > 0 {
		log.Printf("abuse limits: dropped %d new connections or flows, %d UDP packets", f, u)
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
