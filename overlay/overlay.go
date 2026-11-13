// Package overlay wraps tsnet so the Miner and Connect prototypes join the
// Headscale-coordinated network the same way.
package overlay

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/netip"
	"os"
	"strings"

	"tailscale.com/ipn/ipnstate"
	"tailscale.com/tailcfg"
	"tailscale.com/tsnet"
)

// Config is the common overlay configuration, registered as flags.
type Config struct {
	ControlURL string
	AuthKey    string
	Hostname   string
	StateDir   string
	Verbose    bool
}

// RegisterFlags binds the overlay flags. The auth key defaults to
// $NODEEX_AUTHKEY so it stays out of shell history.
func (c *Config) RegisterFlags(fs *flag.FlagSet, defaultHostname, defaultStateDir string) {
	fs.StringVar(&c.ControlURL, "control-url", os.Getenv("NODEEX_CONTROL_URL"), "Headscale URL (or $NODEEX_CONTROL_URL)")
	fs.StringVar(&c.AuthKey, "auth-key", os.Getenv("NODEEX_AUTHKEY"), "Headscale pre-auth key (or $NODEEX_AUTHKEY)")
	fs.StringVar(&c.Hostname, "hostname", defaultHostname, "node name on the overlay network")
	fs.StringVar(&c.StateDir, "state-dir", defaultStateDir, "directory for overlay identity and state")
	fs.BoolVar(&c.Verbose, "verbose", false, "log tsnet internals")
}

// Start joins the overlay and blocks until the node is up.
func Start(ctx context.Context, c Config, logf func(string, ...any)) (*tsnet.Server, error) {
	if c.ControlURL == "" {
		return nil, errors.New("missing -control-url")
	}
	if err := os.MkdirAll(c.StateDir, 0o700); err != nil {
		return nil, err
	}
	srv := &tsnet.Server{
		Hostname:   c.Hostname,
		Dir:        c.StateDir,
		ControlURL: c.ControlURL,
		AuthKey:    c.AuthKey,
		UserLogf:   logf,
	}
	if c.Verbose {
		srv.Logf = logf
	}
	st, err := srv.Up(ctx)
	if err != nil {
		srv.Close()
		return nil, fmt.Errorf("join overlay: %w", err)
	}
	logf("overlay up: %s %v", c.Hostname, st.TailscaleIPs)
	return srv, nil
}

// FindPeer returns the overlay address of the peer named host.
func FindPeer(ctx context.Context, srv *tsnet.Server, host string) (netip.Addr, error) {
	lc, err := srv.LocalClient()
	if err != nil {
		return netip.Addr{}, err
	}
	st, err := lc.Status(ctx)
	if err != nil {
		return netip.Addr{}, err
	}
	for _, p := range st.Peer {
		name, _, _ := strings.Cut(p.DNSName, ".")
		if !strings.EqualFold(p.HostName, host) && !strings.EqualFold(name, host) {
			continue
		}
		if len(p.TailscaleIPs) == 0 {
			return netip.Addr{}, fmt.Errorf("peer %q has no address", host)
		}
		return p.TailscaleIPs[0], nil
	}
	return netip.Addr{}, fmt.Errorf("peer %q not found (not registered, or blocked by ACL)", host)
}

// Path describes how traffic currently reaches a peer.
type Path struct {
	Direct   bool
	Endpoint string // peer's UDP endpoint when direct
	Relay    string // DERP region code when relayed
	Latency  float64
}

func (p Path) String() string {
	if p.Direct {
		return fmt.Sprintf("direct via %s, %.1f ms", p.Endpoint, p.Latency*1000)
	}
	return fmt.Sprintf("relayed via DERP %q, %.1f ms", p.Relay, p.Latency*1000)
}

// Probe pings the peer over the overlay and reports the path taken.
func Probe(ctx context.Context, srv *tsnet.Server, peer netip.Addr) (Path, error) {
	lc, err := srv.LocalClient()
	if err != nil {
		return Path{}, err
	}
	res, err := lc.Ping(ctx, peer, tailcfg.PingDisco)
	if err != nil {
		return Path{}, err
	}
	return pathFromPing(res)
}

func pathFromPing(res *ipnstate.PingResult) (Path, error) {
	if res.Err != "" {
		return Path{}, errors.New(res.Err)
	}
	return Path{
		Direct:   res.Endpoint != "",
		Endpoint: res.Endpoint,
		Relay:    res.DERPRegionCode,
		Latency:  res.LatencySeconds,
	}, nil
}
