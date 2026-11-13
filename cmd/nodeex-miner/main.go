// nodeex-miner is the node-side agent. It enrolls this machine with the
// platform and then keeps it reported and online.
//
//	nodeex-miner check
//	nodeex-miner enroll -api URL -token TOKEN [-name NAME] [-dir DIR]
//	nodeex-miner run [-dir DIR]
//	nodeex-miner status [-dir DIR]
//	nodeex-miner service <install|uninstall|status> [-dir DIR] [-- run flags]
package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"tailscale.com/tsnet"

	"github.com/nodeexhq/miner/internal/devicesig"
	"github.com/nodeexhq/miner/internal/hostinfo"
	"github.com/nodeexhq/miner/internal/hostspec"
	"github.com/nodeexhq/miner/internal/requirements"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

const (
	// The heartbeat is also how the Miner learns a lease was ended early, so
	// this bounds how long a terminated renter stays connected.
	heartbeatInterval = 15 * time.Second
	// How often a Miner without remote access tries again to join the
	// overlay.
	gateRetryInterval = 5 * time.Minute
	// isolationLimit is how long the Miner may be unable to reach the
	// platform before it stops serving its lease. It matches the point at
	// which the platform starts treating the node as offline.
	isolationLimit = 3 * time.Minute
	reportInterval = time.Hour
)

// identity is the Miner's persistent state. The private key never leaves
// this machine.
type identity struct {
	API        string `json:"api"`
	NodeID     string `json:"node_id"`
	PrivateKey string `json:"private_key"` // base64 Ed25519 seed
	// PlatformKey is the platform's lease-token key, pinned at enrollment.
	// Lease tokens are only ever checked against this pinned copy, so a
	// later compromise of the API transport cannot swap it.
	PlatformKey string `json:"platform_key"`
}

func (id identity) key() (ed25519.PrivateKey, error) {
	seed, err := base64.StdEncoding.DecodeString(id.PrivateKey)
	if err != nil || len(seed) != ed25519.SeedSize {
		return nil, errors.New("identity file has a corrupt private key")
	}
	return ed25519.NewKeyFromSeed(seed), nil
}

func identityPath(dir string) string { return filepath.Join(dir, "identity.json") }

func loadIdentity(dir string) (identity, error) {
	var id identity
	b, err := os.ReadFile(identityPath(dir))
	if errors.Is(err, os.ErrNotExist) {
		return id, errors.New("this machine is not enrolled; run `nodeex-miner enroll` first")
	}
	if err != nil {
		return id, err
	}
	return id, json.Unmarshal(b, &id)
}

func defaultDir() string {
	if d, err := os.UserConfigDir(); err == nil {
		return filepath.Join(d, "NodeEx", "miner")
	}
	return "nodeex-miner"
}

func main() {
	log.SetFlags(log.LstdFlags)
	if len(os.Args) < 2 {
		usage()
	}
	var err error
	switch os.Args[1] {
	case "check":
		err = cmdCheck()
	case "enroll":
		err = cmdEnroll(os.Args[2:])
	case "run":
		err = cmdRun(os.Args[2:])
	case "status":
		err = cmdStatus(os.Args[2:])
	case "service":
		err = cmdService(os.Args[2:])
	case "version":
		fmt.Println(version)
	default:
		usage()
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "nodeex-miner:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: nodeex-miner <check|enroll|run|status|service|version> [flags]")
	os.Exit(2)
}

// statusError is a request that got an HTTP answer, just not a successful
// one. Getting any answer at all means the far end was reached over TLS.
type statusError struct {
	Status int
	msg    string
}

func (e *statusError) Error() string { return e.msg }

// answered reports whether a failed request nevertheless reached the
// platform's front door. A refusal, or an error page from the edge when the
// platform itself is down, both count: this machine's link to the platform
// is fine. A failure to connect at all does not.
func answered(err error) bool {
	var se *statusError
	return errors.As(err, &se)
}

// call sends a device-signed JSON request and decodes a JSON response.
func call(ctx context.Context, api string, priv ed25519.PrivateKey, method, path string, in, out any) error {
	var body []byte
	if in != nil {
		var err error
		if body, err = json.Marshal(in); err != nil {
			return err
		}
	}
	req, err := http.NewRequestWithContext(ctx, method, api+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	ts, sig := devicesig.Sign(priv, method, path, body, time.Now())
	req.Header.Set(devicesig.HeaderTimestamp, ts)
	req.Header.Set(devicesig.HeaderSignature, sig)
	req.Header.Set("Content-Type", "application/json")

	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if res.StatusCode >= 300 {
		var e struct {
			Error struct{ Code, Message string } `json:"error"`
		}
		if json.Unmarshal(raw, &e) == nil && e.Error.Code != "" {
			return &statusError{res.StatusCode, fmt.Sprintf("%s %s: %s (%s)", method, path, e.Error.Message, e.Error.Code)}
		}
		return &statusError{res.StatusCode, fmt.Sprintf("%s %s: HTTP %d", method, path, res.StatusCode)}
	}
	if out != nil && len(raw) > 0 {
		return json.Unmarshal(raw, out)
	}
	return nil
}

func describe(h hostspec.HostSpecs) string {
	kind := "desktop"
	if h.HasBattery {
		kind = "laptop"
	}
	return fmt.Sprintf("%s (%s, %s), %d cores, %d GB memory, %d GB disk (%d GB free), macOS %s",
		h.Chip, h.Arch, kind, h.CPUCores, h.MemoryGB, h.DiskTotalGB, h.DiskFreeGB, h.MacOS)
}

// printCheck reports whether this machine meets the minimum hardware and
// returns false if it does not.
func printCheck(h hostspec.HostSpecs) bool {
	fmt.Println("this machine:", describe(h))
	failed := requirements.Check(h)
	if len(failed) == 0 {
		fmt.Println("meets the minimum requirements to be a NodeEx node")
		return true
	}
	fmt.Println("does NOT meet the minimum requirements:")
	for _, f := range failed {
		fmt.Println("  -", f.Message)
	}
	return false
}

func cmdCheck() error {
	if !printCheck(hostinfo.Collect()) {
		os.Exit(1)
	}
	return nil
}

func cmdEnroll(args []string) error {
	fs := flag.NewFlagSet("enroll", flag.ExitOnError)
	api := fs.String("api", os.Getenv("NODEEX_API"), "platform API URL (or $NODEEX_API)")
	token := fs.String("token", os.Getenv("NODEEX_ENROLL_TOKEN"), "enrollment token from the provider console (or $NODEEX_ENROLL_TOKEN)")
	host, _ := os.Hostname()
	name := fs.String("name", host, "display name for this node")
	dir := fs.String("dir", defaultDir(), "state directory")
	force := fs.Bool("force", false, "try to enroll even if this machine is below the minimum requirements (the platform may still refuse)")
	fs.Parse(args)

	if *api == "" || *token == "" {
		return errors.New("enroll needs -api and -token")
	}
	if _, err := os.Stat(identityPath(*dir)); err == nil {
		return fmt.Errorf("already enrolled (%s exists); remove it to enroll again", identityPath(*dir))
	}
	// Check before spending the single-use token on an enrollment the
	// platform would refuse anyway.
	if !printCheck(hostinfo.Collect()) && !*force {
		return errors.New("not enrolling")
	}

	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return err
	}
	specs := hostinfo.Collect()
	req := map[string]any{
		"enroll_token":  *token,
		"public_key":    base64.StdEncoding.EncodeToString(pub),
		"name":          *name,
		"miner_version": version,
		"host":          specs,
	}
	var res struct {
		NodeID string `json:"node_id"`
		Status string `json:"status"`
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var keys struct {
		LeaseTokenKey string `json:"lease_token_key"`
	}
	if err := call(ctx, *api, priv, "GET", "/v1/platform/keys", nil, &keys); err != nil {
		return err
	}
	if k, err := base64.StdEncoding.DecodeString(keys.LeaseTokenKey); err != nil || len(k) != ed25519.PublicKeySize {
		return errors.New("platform returned an invalid lease token key")
	}
	if err := call(ctx, *api, priv, "POST", "/v1/nodes/enroll", req, &res); err != nil {
		return err
	}

	id := identity{
		API: *api, NodeID: res.NodeID,
		PrivateKey:  base64.StdEncoding.EncodeToString(priv.Seed()),
		PlatformKey: keys.LeaseTokenKey,
	}
	b, _ := json.MarshalIndent(id, "", "  ")
	if err := os.MkdirAll(*dir, 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(identityPath(*dir), b, 0o600); err != nil {
		return fmt.Errorf("enrolled as %s but could not save identity: %w", res.NodeID, err)
	}
	fmt.Printf("enrolled: node %s (%s)\n", res.NodeID, res.Status)
	return nil
}

func cmdStatus(args []string) error {
	fs := flag.NewFlagSet("status", flag.ExitOnError)
	dir := fs.String("dir", defaultDir(), "state directory")
	fs.Parse(args)
	id, err := loadIdentity(*dir)
	if err != nil {
		return err
	}
	fmt.Printf("node:  %s\napi:   %s\nstate: %s\n", id.NodeID, id.API, *dir)
	return nil
}

func cmdRun(args []string) error {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	dir := fs.String("dir", defaultDir(), "state directory")
	paused := fs.Bool("paused", false, "stay online but do not accept new leases")
	sshTarget := fs.String("ssh-target", "127.0.0.1:22", "where renters' SSH connections are forwarded")
	vncTarget := fs.String("vnc-target", "127.0.0.1:5900", "where renters' remote desktop connections are forwarded")
	machineCPU := fs.Int("machine-cpus", 0, "cores of the virtual machine each lease is given (with -machine-memory-gb and -machine-disk-gb)")
	machineMemory := fs.Int("machine-memory-gb", 0, "memory of that machine")
	machineDisk := fs.Int("machine-disk-gb", 0, "disk of that machine")
	events := fs.Bool("events", false, "write state changes to standard output as JSON lines, for a program that supervises the Miner")
	fs.Parse(args)
	emit := eventWriter(*events)
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if *events {
		// Supervised: if the supervisor dies, so does the machine it was
		// running, and a Miner left behind would report a node that can
		// serve nobody. Leave with it.
		parent := os.Getppid()
		go func() {
			for range time.Tick(time.Second) {
				if os.Getppid() != parent {
					log.Printf("the supervising program is gone; stopping")
					cancel()
					return
				}
			}
		}()
	}

	id, err := loadIdentity(*dir)
	if err != nil {
		return err
	}
	priv, err := id.key()
	if err != nil {
		return err
	}
	report := func() {
		req := struct {
			MinerVersion    string             `json:"miner_version"`
			AcceptingLeases bool               `json:"accepting_leases"`
			Host            hostspec.HostSpecs `json:"host"`
		}{version, !*paused, hostinfo.Collect()}
		// The market describes what a renter gets, so a node that gives
		// each lease its own machine says how large that machine is.
		if *machineCPU > 0 && *machineMemory > 0 && *machineDisk > 0 {
			req.Host.MachineCPUCores, req.Host.MachineMemoryGB, req.Host.MachineDiskGB = *machineCPU, *machineMemory, *machineDisk
		}
		cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		if err := call(cctx, id.API, priv, "POST", "/v1/nodes/"+id.NodeID+"/reports", req, nil); err != nil {
			log.Printf("report failed: %v", err)
			return
		}
		log.Printf("report sent")
	}
	// Join the overlay and open the gate. The platform gives a join key only
	// to an approved node, and may have no overlay configured at all, so
	// this can fail: the Miner then still reports and heartbeats, nobody can
	// connect to it, and it tries again from the main loop.
	state := &leaseState{}
	myAddr := ""
	var srv *tsnet.Server
	defer func() {
		if srv != nil {
			srv.Close()
		}
	}()
	lastJoin := time.Time{}
	openGate := func() error {
		lastJoin = time.Now()
		jctx, jcancel := context.WithTimeout(ctx, 60*time.Second)
		joined, key, err := joinOverlay(jctx, id, priv, *dir)
		jcancel()
		if err != nil {
			return err
		}
		addr, err := overlayAddr(ctx, joined)
		if err != nil {
			joined.Close()
			return err
		}
		ln, err := gateListen(joined, key.GatePort)
		if err != nil {
			joined.Close()
			return err
		}
		srv, myAddr = joined, addr
		log.Printf("gate listening on %s:%d (ssh -> %s, vnc -> %s)", myAddr, key.GatePort, *sshTarget, *vncTarget)
		emit(event{Event: "gate", State: "open", Addr: myAddr})
		go func() {
			if err := serveGate(ctx, ln, id, state, gateTargets(*sshTarget, *vncTarget)); err != nil {
				log.Printf("gate stopped: %v", err)
				cancel()
			}
		}()
		return nil
	}
	if err := openGate(); err != nil {
		log.Printf("overlay unavailable, running without remote access for now: %v", err)
	}

	lastStatus := ""
	lastKeys := ""
	started := time.Now()
	lastReached := time.Now()
	isolated := false
	heartbeat := func() {
		var res struct {
			Status string `json:"status"`
			Lease  *struct {
				ID      string    `json:"id"`
				EndsAt  time.Time `json:"ends_at"`
				SSHKeys []string  `json:"ssh_keys"`
			} `json:"lease"`
		}
		cctx, cancel := context.WithTimeout(ctx, 15*time.Second)
		defer cancel()
		// Every heartbeat carries the Miner's own account of how it is
		// running, for the owner's dashboard and for operators.
		sessions, suspended := state.snapshot()
		gate := "open"
		switch {
		case srv == nil:
			gate = "off"
		case suspended:
			gate = "suspended"
		}
		req := map[string]any{
			"overlay_addr": myAddr,
			"status": map[string]any{
				"miner_version":  version,
				"uptime_seconds": int64(time.Since(started).Seconds()),
				"load1":          hostinfo.Load1(),
				"disk_free_gb":   hostinfo.Collect().DiskFreeGB,
				"sessions":       sessions,
				"gate":           gate,
			},
		}
		if err := call(cctx, id.API, priv, "POST", "/v1/nodes/"+id.NodeID+"/heartbeat", req, &res); err != nil {
			if ctx.Err() != nil {
				return
			}
			log.Printf("heartbeat failed: %v", err)
			// If the platform answered, even with an error, it is the
			// platform that is in trouble: keep serving the lease we know
			// about, whose end time is enforced locally, so its outage
			// does not cut off a paying renter.
			if answered(err) {
				lastReached = time.Now()
				return
			}
			// If this machine cannot reach the platform at all, the
			// platform will soon record it as offline and stop charging
			// the renter. It must then really be out of service: without
			// this, someone with control of the machine could block the
			// platform's address alone and go on using it for free.
			if time.Since(lastReached) > isolationLimit && state.suspend() {
				isolated = true
				log.Printf("no contact with the platform for %s; remote access is suspended until it returns", isolationLimit)
				emit(event{Event: "gate", State: "suspended", Addr: myAddr})
			}
			return
		}
		lastReached = time.Now()
		if isolated {
			isolated = false
			log.Printf("contact with the platform restored; remote access resumes")
			emit(event{Event: "gate", State: "open", Addr: myAddr})
		}
		changed := res.Status != lastStatus
		if changed {
			log.Printf("node status: %s", res.Status)
			emit(event{Event: "node", State: res.Status, Node: id.NodeID})
			lastStatus = res.Status
		}
		// A node that was waiting for review when the Miner started could
		// not join the overlay then. Join as soon as it is approved, and
		// otherwise keep trying now and then.
		if srv == nil && ctx.Err() == nil && (changed && res.Status == "active" || time.Since(lastJoin) > gateRetryInterval) {
			if err := openGate(); err != nil {
				log.Printf("overlay still unavailable: %v", err)
			}
		}
		lease, endsAt := "", time.Time{}
		var keys []string
		if res.Lease != nil {
			lease, endsAt, keys = res.Lease.ID, res.Lease.EndsAt, res.Lease.SSHKeys
		}
		if previous := state.set(lease, endsAt); previous != lease {
			if previous != "" {
				log.Printf("lease %s ended; its connections are closed", previous)
				emit(event{Event: "lease_ended", Lease: previous})
			}
			if lease != "" {
				log.Printf("lease %s started, ends %s", lease, endsAt.Local().Format(time.DateTime))
				emit(event{Event: "lease_started", Lease: lease, EndsAt: &endsAt})
			}
			lastKeys = ""
		}
		// The renter's keys can arrive, or change, after the lease starts.
		if joined := strings.Join(keys, "\n"); lease != "" && joined != lastKeys {
			lastKeys = joined
			emit(event{Event: "lease_keys", Lease: lease, SSHKeys: append([]string{}, keys...)})
		}
	}

	probe := func() {
		summary, err := runProbe(ctx, id, priv)
		if err != nil {
			if ctx.Err() == nil {
				log.Printf("speed probe skipped: %v", err)
			}
			return
		}
		log.Printf("speed probe: %s", summary)
	}

	log.Printf("nodeex-miner %s running as node %s", version, id.NodeID)
	report()
	heartbeat()
	// Off the main loop: a probe can take half a minute and must not
	// delay heartbeats.
	go probe()
	hb := time.NewTicker(heartbeatInterval)
	defer hb.Stop()
	rp := time.NewTicker(reportInterval)
	defer rp.Stop()
	expiry := time.NewTicker(time.Second)
	defer expiry.Stop()
	pr := time.NewTicker(probeInterval)
	defer pr.Stop()
	for {
		select {
		case <-pr.C:
			go probe()
		case <-expiry.C:
			state.enforceExpiry()
		case <-ctx.Done():
			log.Printf("stopping")
			return nil
		case <-hb.C:
			heartbeat()
		case <-rp.C:
			report()
		}
	}
}
