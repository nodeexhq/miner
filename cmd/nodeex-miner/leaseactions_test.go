package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nodeexhq/miner/internal/devicesig"
)

// platform is a fake API that checks each request's device signature and
// answers from a script.
type platform struct {
	t      *testing.T
	pub    ed25519.PublicKey
	mu     sync.Mutex
	script []reply
	got    []recorded
}

type reply struct {
	status int
	body   string
}

type recorded struct {
	path string
	body map[string]any
}

func (p *platform) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	if err := devicesig.Verify(p.pub, r.Method, r.URL.Path, body,
		r.Header.Get(devicesig.HeaderTimestamp), r.Header.Get(devicesig.HeaderSignature), time.Now()); err != nil {
		p.t.Errorf("%s %s: signature: %v", r.Method, r.URL.Path, err)
		http.Error(w, `{"error":{"code":"invalid_signature","message":"bad"}}`, 401)
		return
	}
	var m map[string]any
	json.Unmarshal(body, &m)
	p.mu.Lock()
	defer p.mu.Unlock()
	p.got = append(p.got, recorded{r.URL.Path, m})
	next := reply{200, "{}"}
	if len(p.script) > 0 {
		next, p.script = p.script[0], p.script[1:]
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(next.status)
	io.WriteString(w, next.body)
}

func newPlatform(t *testing.T, script ...reply) (*platform, poster) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	p := &platform{t: t, pub: pub, script: script}
	srv := httptest.NewServer(p)
	t.Cleanup(srv.Close)
	post, err := devicePoster(identity{API: srv.URL, NodeID: "node-1", PrivateKey: base64.StdEncoding.EncodeToString(priv.Seed())})
	if err != nil {
		t.Fatal(err)
	}
	return p, post
}

// noSleep records the waits instead of waiting.
type noSleep struct{ waits []time.Duration }

func (n *noSleep) sleep(ctx context.Context, d time.Duration) error {
	n.waits = append(n.waits, d)
	return ctx.Err()
}

const notCurrent = `{"error":{"code":"lease_not_current","message":"the lease is not the one this node is serving"}}`

func TestAbuseRequest(t *testing.T) {
	req, err := newAbuseRequest("lease-1", "port_scan", "  more than 64 ports of 10.0.0.9 in 60 s\n")
	if err != nil || req != (abuseRequest{"lease-1", "port_scan", "more than 64 ports of 10.0.0.9 in 60 s"}) {
		t.Errorf("%+v %v", req, err)
	}
	if _, err := newAbuseRequest("lease-1", "mining", ""); exitCode(err) != 2 {
		t.Errorf("unknown reason: %v", err)
	}
	if _, err := newAbuseRequest("", "fan_out", ""); exitCode(err) != 2 {
		t.Errorf("no lease: %v", err)
	}
	// Too long a detail is cut, on a character boundary, not refused.
	long, _ := newAbuseRequest("lease-1", "fan_out", strings.Repeat("é", 600))
	if len(long.Detail) != maxAbuseDetail || !strings.HasPrefix(strings.Repeat("é", 600), long.Detail) {
		t.Errorf("detail of %d bytes", len(long.Detail))
	}
	odd := truncate("a"+strings.Repeat("é", 600), maxAbuseDetail)
	if len(odd) != maxAbuseDetail-1 {
		t.Errorf("split character: %d bytes", len(odd))
	}
}

func TestReportAbuseRetries(t *testing.T) {
	p, post := newPlatform(t,
		reply{503, `{"error":{"code":"unavailable","message":"down"}}`},
		reply{502, "bad gateway"},
		reply{429, `{"error":{"code":"rate_limited","message":"slow down"}}`},
		reply{200, `{"lease_id":"lease-1","reason":"fan_out","ended_at":"2026-11-21T10:00:00Z","paid":"1.23"}`},
	)
	var s noSleep
	req, _ := newAbuseRequest("lease-1", "fan_out", "more than 32 hosts in 60 s")
	got, err := reportAbuse(context.Background(), post, "node-1", req, s.sleep)
	if err != nil || got.Paid != "1.23" || got.LeaseID != "lease-1" {
		t.Fatalf("%+v %v", got, err)
	}
	if len(p.got) != 4 {
		t.Fatalf("%d attempts", len(p.got))
	}
	for _, r := range p.got {
		if r.path != "/v1/nodes/node-1/abuse" || r.body["lease_id"] != "lease-1" || r.body["reason"] != "fan_out" || r.body["detail"] != "more than 32 hosts in 60 s" {
			t.Errorf("request %+v", r)
		}
	}
	if want := []time.Duration{2 * time.Second, 4 * time.Second, 8 * time.Second}; !equal(s.waits, want) {
		t.Errorf("waits %v, want %v", s.waits, want)
	}
}

func equal(a, b []time.Duration) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestReportAbuseStops(t *testing.T) {
	req, _ := newAbuseRequest("lease-1", "half_open", "")
	for name, c := range map[string]struct {
		reply reply
		code  int
	}{
		"lease already ended": {reply{409, notCurrent}, exitLeaseNotCurrent},
		"refused":             {reply{400, `{"error":{"code":"invalid_reason","message":"no"}}`}, exitRefused},
		"not this node's":     {reply{404, `{"error":{"code":"not_found","message":"no"}}`}, exitRefused},
	} {
		p, post := newPlatform(t, c.reply)
		var s noSleep
		_, err := reportAbuse(context.Background(), post, "node-1", req, s.sleep)
		if exitCode(err) != c.code || len(p.got) != 1 || len(s.waits) != 0 {
			t.Errorf("%s: %v (exit %d), %d attempts, waits %v", name, err, exitCode(err), len(p.got), s.waits)
		}
	}
}

// Backoff grows to a minute and stops when the time allowed is over.
func TestReportAbuseGivesUp(t *testing.T) {
	var script []reply
	for range 20 {
		script = append(script, reply{500, "oops"})
	}
	p, post := newPlatform(t, script...)
	ctx, cancel := context.WithCancel(context.Background())
	var waits []time.Duration
	sleep := func(ctx context.Context, d time.Duration) error {
		waits = append(waits, d)
		if len(waits) == 8 {
			cancel()
		}
		return ctx.Err()
	}
	req, _ := newAbuseRequest("lease-1", "sustained_udp", "")
	_, err := reportAbuse(ctx, post, "node-1", req, sleep)
	if err == nil || exitCode(err) != exitFailed || len(p.got) != 8 {
		t.Fatalf("%v (exit %d) after %d attempts", err, exitCode(err), len(p.got))
	}
	want := []time.Duration{2, 4, 8, 16, 32, 60, 60, 60}
	for i := range want {
		want[i] *= time.Second
	}
	if !equal(waits, want) {
		t.Errorf("waits %v", waits)
	}

	// No platform at all: the same.
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	post, _ = devicePoster(identity{API: "http://127.0.0.1:1", NodeID: "node-1", PrivateKey: base64.StdEncoding.EncodeToString(priv.Seed())})
	var s noSleep
	ctx, cancel = context.WithCancel(context.Background())
	cancel()
	if _, err := reportAbuse(ctx, post, "node-1", req, s.sleep); exitCode(err) != exitFailed || len(s.waits) != 1 {
		t.Errorf("unreachable: %v, waits %v", err, s.waits)
	}
}

func TestStopLease(t *testing.T) {
	p, post := newPlatform(t, reply{200, `{"lease_id":"lease-1","ended_at":"2026-11-21T10:00:00Z","refunded":"12.50","mining":false,"paused_until":"2026-11-28T10:00:00Z"}`})
	got, err := stopLease(context.Background(), post, "node-1", "lease-1", "  the renter is using it for something I do not allow  ")
	if err != nil || got.Refunded != "12.50" || got.Mining || got.PausedUntil == nil {
		t.Fatalf("%+v %v", got, err)
	}
	if r := p.got[0]; r.path != "/v1/nodes/node-1/stop-lease" || r.body["lease_id"] != "lease-1" ||
		r.body["reason"] != "the renter is using it for something I do not allow" {
		t.Errorf("request %+v", r)
	}
	// The reply is printed for the app as one JSON line.
	b, _ := json.Marshal(got)
	if !strings.Contains(string(b), `"refunded":"12.50"`) || !strings.Contains(string(b), `"mining":false`) {
		t.Errorf("printed %s", b)
	}

	_, post = newPlatform(t, reply{409, notCurrent})
	if _, err := stopLease(context.Background(), post, "node-1", "lease-1", ""); exitCode(err) != exitLeaseNotCurrent {
		t.Errorf("lease ended: %v", err)
	}
	// Asked once only: the owner is waiting.
	p, post = newPlatform(t, reply{503, "down"})
	if _, err := stopLease(context.Background(), post, "node-1", "lease-1", ""); exitCode(err) != exitFailed || len(p.got) != 1 {
		t.Errorf("platform down: %v, %d attempts", err, len(p.got))
	}
	if long := truncate(strings.Repeat("x", 900), maxStopReason); len(long) != maxStopReason {
		t.Errorf("reason of %d bytes", len(long))
	}
}

func TestExitCode(t *testing.T) {
	if exitCode(errors.New("x")) != 1 || exitCode(&exitError{3, errors.New("x")}) != 3 {
		t.Error("exit codes")
	}
}
