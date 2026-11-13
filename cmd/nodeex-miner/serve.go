package main

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/nodeexhq/miner/gate"
	"github.com/nodeexhq/miner/leasetoken"
	"github.com/nodeexhq/miner/overlay"
	"github.com/nodeexhq/miner/tunnel"
	"tailscale.com/tsnet"
)

// leaseState is what the Miner currently knows about its lease, plus every
// connection it is carrying for it.
type leaseState struct {
	mu     sync.Mutex
	id     string
	endsAt time.Time
	conns  map[net.Conn]struct{}
	// suspended is set while the Miner is cut off from the platform. No
	// lease is served until the platform is heard from again.
	suspended bool
}

// snapshot reports how many connections are being carried and whether the
// gate is suspended, for the Miner's status report.
func (s *leaseState) snapshot() (sessions int, suspended bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.conns), s.suspended
}

// suspend stops serving until the next word from the platform, cutting
// every connection. It reports whether this call changed anything.
func (s *leaseState) suspend() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.suspended {
		return false
	}
	s.suspended = true
	s.dropLocked()
	return true
}

// endGrace is how long past a lease's known end time the Miner keeps serving
// it. A renewal reaches the Miner on its next heartbeat, and the two clocks
// are never exactly in step; without this margin a renter whose lease was
// renewed at the last moment would be cut off for nothing. When the platform
// says the lease is over, access stops at once regardless.
const endGrace = 90 * time.Second

func (s *leaseState) expired(now time.Time) bool {
	return !now.Before(s.endsAt.Add(endGrace))
}

// current returns the lease the Miner may serve right now. The lease's end
// time is enforced locally, so access stops even if the platform is
// unreachable.
func (s *leaseState) current() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.suspended || s.id == "" || s.expired(time.Now()) {
		return ""
	}
	return s.id
}

// set records the lease reported by the platform ("" for none). When the
// lease changes or ends, every connection carried for the old one is cut.
func (s *leaseState) set(id string, endsAt time.Time) (previous string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	previous = s.id
	if id != s.id {
		s.dropLocked()
	}
	s.id, s.endsAt = id, endsAt
	// Hearing from the platform ends any suspension.
	s.suspended = false
	return previous
}

// enforceExpiry cuts connections once the known lease has run out.
func (s *leaseState) enforceExpiry() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.id != "" && s.expired(time.Now()) {
		log.Printf("lease %s reached its end time; closing %d connection(s)", s.id, len(s.conns))
		s.dropLocked()
		s.id = ""
	}
}

func (s *leaseState) dropLocked() {
	for c := range s.conns {
		c.Close()
	}
	s.conns = nil
}

// track registers conn under the given lease. It refuses if that lease is no
// longer the current one, closing the gap between the handshake and here.
func (s *leaseState) track(lease string, conn net.Conn) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.suspended || s.id != lease || s.expired(time.Now()) {
		return false
	}
	if s.conns == nil {
		s.conns = make(map[net.Conn]struct{})
	}
	s.conns[conn] = struct{}{}
	return true
}

func (s *leaseState) untrack(conn net.Conn) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.conns, conn)
}

// maxHandshakes is how many connections may be in the gate handshake at the
// same time. A real client finishes its handshake in one round trip.
const maxHandshakes = 32

type overlayKey struct {
	ControlURL string `json:"control_url"`
	AuthKey    string `json:"auth_key"`
	GatePort   int    `json:"gate_port"`
}

// joinOverlay brings the Miner onto the overlay network. A join key is
// requested from the platform only the first time. After that the identity
// in the state directory is reused, together with the settings saved beside
// it, so restarting a Miner does not mint another key.
func joinOverlay(ctx context.Context, id identity, priv ed25519.PrivateKey, dir string) (*tsnet.Server, overlayKey, error) {
	stateDir := filepath.Join(dir, "overlay")
	settingsPath := filepath.Join(dir, "overlay.json")

	var key overlayKey
	if b, err := os.ReadFile(settingsPath); err == nil && json.Unmarshal(b, &key) == nil && key.ControlURL != "" {
		if entries, _ := os.ReadDir(stateDir); len(entries) > 0 {
			cfg := overlay.Config{ControlURL: key.ControlURL, Hostname: "n-" + id.NodeID, StateDir: stateDir}
			srv, err := overlay.Start(ctx, cfg, log.Printf)
			return srv, key, err
		}
	}

	if err := call(ctx, id.API, priv, "POST", "/v1/nodes/"+id.NodeID+"/overlay-key", nil, &key); err != nil {
		return nil, key, err
	}
	cfg := overlay.Config{ControlURL: key.ControlURL, AuthKey: key.AuthKey, Hostname: "n-" + id.NodeID, StateDir: stateDir}
	srv, err := overlay.Start(ctx, cfg, log.Printf)
	if err != nil {
		return nil, key, err
	}
	// The join key is single-use and now spent; only the settings are kept.
	saved, _ := json.Marshal(overlayKey{ControlURL: key.ControlURL, GatePort: key.GatePort})
	if err := os.WriteFile(settingsPath, saved, 0o600); err != nil {
		log.Printf("could not save overlay settings: %v", err)
	}
	return srv, key, nil
}

// overlayAddr returns the Miner's own IPv4 address on the overlay.
func overlayAddr(ctx context.Context, srv *tsnet.Server) (string, error) {
	lc, err := srv.LocalClient()
	if err != nil {
		return "", err
	}
	st, err := lc.Status(ctx)
	if err != nil {
		return "", err
	}
	for _, ip := range st.TailscaleIPs {
		if ip.Is4() {
			return ip.String(), nil
		}
	}
	return "", errors.New("overlay assigned no IPv4 address")
}

// serveGate accepts overlay connections, admits those holding a token for
// the current lease, and forwards them to the leased machine's services.
func serveGate(ctx context.Context, ln net.Listener, id identity, state *leaseState, targets map[string]string) error {
	platformKey, err := base64.StdEncoding.DecodeString(id.PlatformKey)
	if err != nil || len(platformKey) != ed25519.PublicKeySize {
		return errors.New("identity file has no valid platform key; enroll again")
	}
	verifier := gate.Verifier{PlatformKey: platformKey, NodeID: id.NodeID, CurrentLease: state.current}

	// Anyone on the overlay can open connections to the gate. Bound how
	// many may be mid-handshake at once, so a flood of idle connections
	// cannot pile up goroutines and sockets and starve the real renter.
	handshakes := make(chan struct{}, maxHandshakes)

	stop := context.AfterFunc(ctx, func() { ln.Close() })
	defer stop()
	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		select {
		case handshakes <- struct{}{}:
		default:
			conn.Close()
			continue
		}
		go func() {
			defer conn.Close()
			service, claims, err := verifier.Accept(conn)
			<-handshakes
			if err != nil {
				log.Printf("gate: refused %s: %v", conn.RemoteAddr(), err)
				return
			}
			if !state.track(claims.LeaseID, conn) {
				return
			}
			defer state.untrack(conn)

			var d net.Dialer
			up, err := d.DialContext(ctx, "tcp", targets[service])
			if err != nil {
				log.Printf("gate: %s for lease %s: cannot reach %s: %v", service, claims.LeaseID, targets[service], err)
				return
			}
			defer up.Close()
			log.Printf("gate: %s open for lease %s from %s", service, claims.LeaseID, conn.RemoteAddr())
			tunnel.Pipe(ctx, conn, up)
			log.Printf("gate: %s closed for lease %s", service, claims.LeaseID)
		}()
	}
}

func gateTargets(ssh, vnc string) map[string]string {
	return map[string]string{leasetoken.ServiceSSH: ssh, leasetoken.ServiceVNC: vnc}
}

func gateListen(srv *tsnet.Server, port int) (net.Listener, error) {
	return srv.Listen("tcp", fmt.Sprintf(":%d", port))
}
