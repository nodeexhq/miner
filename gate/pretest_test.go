package gate

import (
	"context"
	"errors"
	"net"
	"path/filepath"
	"testing"
	"time"

	"github.com/nvtohq/node/leasetoken"
	"github.com/nvtohq/node/pretest"
)

type testFixture struct {
	*fixture
	path   string
	leased bool
	guard  *pretest.Guard
}

func newTestFixture(t *testing.T) *testFixture {
	f := &testFixture{fixture: newFixture(), path: filepath.Join(t.TempDir(), "pretest.json")}
	f.lease = "" // no lease: the node is free for a test
	f.restart(t)
	return f
}

// restart is a new node process on the same state directory.
func (f *testFixture) restart(t *testing.T) {
	t.Helper()
	g, err := pretest.OpenGuard(f.path, func() bool { return f.leased }, func() time.Time { return f.now })
	if err != nil {
		t.Fatal(err)
	}
	f.guard = g
}

func (f *testFixture) verifier() Verifier {
	v := f.fixture.verifier()
	v.Tests = f.guard
	return v
}

// testToken issues a test token, after edit has had its way with the claims.
func (f *testFixture) testToken(t *testing.T, edit func(*leasetoken.Claims)) string {
	t.Helper()
	lim := leasetoken.DefaultTestLimits()
	c := leasetoken.Claims{
		NodeID: "node-1", Services: []string{leasetoken.ServiceTest}, Scope: leasetoken.ScopeTest,
		TokenID: leasetoken.NewTokenID(), ReservationID: "res-1",
		IssuedAt: f.now.Unix(), ExpiresAt: f.now.Add(leasetoken.TestTTL).Unix(), Test: &lim,
	}
	if edit != nil {
		edit(&c)
	}
	tok, err := leasetoken.Issue(f.priv, c)
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

// handshake runs the gate over a pipe. An admitted test is ended at once.
func (f *testFixture) handshake(service, token string) (Admitted, error, error) {
	c, s := net.Pipe()
	defer c.Close()
	defer s.Close()
	done := make(chan struct{})
	var a Admitted
	var serverErr error
	go func() {
		defer close(done)
		a, serverErr = f.verifier().Accept(s)
		if a.Test != nil {
			a.Test.Done(0)
		}
	}()
	clientErr := Dial(c, service, token)
	<-done
	return a, serverErr, clientErr
}

func refusedWith(err error) string {
	var r *Refused
	if errors.As(err, &r) {
		return r.Reason
	}
	if err == nil {
		return "OK"
	}
	return err.Error()
}

func TestTestTokens(t *testing.T) {
	f := newTestFixture(t)

	a, serverErr, clientErr := f.handshake("test", f.testToken(t, nil))
	if serverErr != nil || clientErr != nil || a.Service != "test" || a.Test == nil || a.Claims.ReservationID != "res-1" {
		t.Fatalf("valid test token: %+v, %v, %v", a, serverErr, clientErr)
	}

	other := newFixture()
	leaseFor := func(services ...string) string {
		tok, _ := leasetoken.Issue(f.priv, leasetoken.Claims{LeaseID: "lease-1", NodeID: "node-1", Services: services,
			IssuedAt: f.now.Unix(), ExpiresAt: f.now.Add(leasetoken.TTL).Unix()})
		return tok
	}
	cases := []struct {
		name, service, token, want string
	}{
		{"no scope", "test", f.testToken(t, func(c *leasetoken.Claims) { c.Scope = "" }), "unauthorized"},
		{"other scope", "test", f.testToken(t, func(c *leasetoken.Claims) { c.Scope = "lease" }), "unauthorized"},
		{"lease token for the test", "test", leaseFor("ssh", "vnc", "test"), "unauthorized"},
		{"test token for ssh", "ssh", f.testToken(t, nil), "unauthorized"},
		{"test token for vnc", "vnc", f.testToken(t, nil), "unauthorized"},
		{"test token granting ssh too, for ssh", "ssh", f.testToken(t, func(c *leasetoken.Claims) { c.Services = []string{"test", "ssh"} }), "unauthorized"},
		{"test token granting ssh too, for the test", "test", f.testToken(t, func(c *leasetoken.Claims) { c.Services = []string{"test", "ssh"} }), "unauthorized"},
		{"test token for another service", "rdp", f.testToken(t, func(c *leasetoken.Claims) { c.Services = []string{"rdp"} }), "unauthorized"},
		{"other node", "test", f.testToken(t, func(c *leasetoken.Claims) { c.NodeID = "node-2" }), "unauthorized"},
		{"other key", "test", (&testFixture{fixture: other}).testToken(t, nil), "unauthorized"},
		{"expired", "test", f.testToken(t, func(c *leasetoken.Claims) { c.IssuedAt -= 300; c.ExpiresAt = f.now.Unix() }), "token-expired"},
		{"lives too long", "test", f.testToken(t, func(c *leasetoken.Claims) { c.ExpiresAt = c.IssuedAt + 301 }), "unauthorized"},
		{"issued in the future", "test", f.testToken(t, func(c *leasetoken.Claims) { c.IssuedAt += 3600; c.ExpiresAt += 3600 }), "unauthorized"},
		{"no token id", "test", f.testToken(t, func(c *leasetoken.Claims) { c.TokenID = "" }), "unauthorized"},
		{"no reservation", "test", f.testToken(t, func(c *leasetoken.Claims) { c.ReservationID = "" }), "unauthorized"},
		{"names a lease", "test", f.testToken(t, func(c *leasetoken.Claims) { c.LeaseID = "lease-1" }), "unauthorized"},
		{"no limits", "test", f.testToken(t, func(c *leasetoken.Claims) { c.Test = nil }), "unauthorized"},
		{"garbage", "test", "garbage", "unauthorized"},
	}
	for _, c := range cases {
		if _, _, clientErr := f.handshake(c.service, c.token); refusedWith(clientErr) != c.want {
			t.Errorf("%s: %s, want %s", c.name, refusedWith(clientErr), c.want)
		}
	}

	// A lease token still works for its lease: tests change nothing there.
	f.lease = "lease-1"
	if a, _, err := f.handshake("ssh", leaseFor("ssh")); err != nil || a.Test != nil {
		t.Errorf("lease token for ssh: %+v %v", a, err)
	}
	// A node that runs no tests refuses them.
	f.guard = nil
	if _, _, err := f.handshake("test", f.testToken(t, nil)); refusedWith(err) != "unauthorized" {
		t.Errorf("node without tests: %v", err)
	}
}

func TestTestTokenSingleUse(t *testing.T) {
	f := newTestFixture(t)
	tok := f.testToken(t, nil)
	if _, _, err := f.handshake("test", tok); err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.handshake("test", tok); refusedWith(err) != "token-used" {
		t.Errorf("replay: %s", refusedWith(err))
	}
	// The node restarts (state on disk) within the token's five minutes.
	f.now = f.now.Add(4 * time.Minute)
	f.restart(t)
	if _, _, err := f.handshake("test", tok); refusedWith(err) != "token-used" {
		t.Errorf("replay after a restart: %s", refusedWith(err))
	}
	// Past its life it is simply expired.
	f.now = f.now.Add(time.Minute)
	if _, _, err := f.handshake("test", tok); refusedWith(err) != "token-expired" {
		t.Errorf("after expiry: %s", refusedWith(err))
	}
}

func TestTestBusy(t *testing.T) {
	f := newTestFixture(t)
	// During a lease, a test is refused and the token kept for later.
	f.leased = true
	tok := f.testToken(t, nil)
	if _, _, err := f.handshake("test", tok); refusedWith(err) != "busy" {
		t.Errorf("during a lease: %s", refusedWith(err))
	}
	f.leased = false

	// During another test.
	c, s := net.Pipe()
	defer c.Close()
	defer s.Close()
	held := make(chan Admitted, 1)
	go func() {
		a, _ := f.verifier().Accept(s)
		held <- a
	}()
	if err := Dial(c, "test", f.testToken(t, nil)); err != nil {
		t.Fatal(err)
	}
	running := <-held
	if _, _, err := f.handshake("test", tok); refusedWith(err) != "busy" {
		t.Errorf("during another test: %s", refusedWith(err))
	}
	running.Test.Done(0)
	if _, _, err := f.handshake("test", tok); err != nil {
		t.Errorf("once free: %v", err)
	}

	// When the hour's budget is short.
	for i := range 10 {
		c, s := net.Pipe()
		done := make(chan struct{})
		go func() {
			defer close(done)
			if a, err := f.verifier().Accept(s); err == nil {
				a.Test.Done(19_000_000)
			}
		}()
		if err := Dial(c, "test", f.testToken(t, nil)); err != nil {
			t.Fatalf("test %d: %v", i, err)
		}
		<-done
		c.Close()
		s.Close()
	}
	if _, _, err := f.handshake("test", f.testToken(t, nil)); refusedWith(err) != "busy" {
		t.Errorf("budget spent (%d left): %s", f.guard.Remaining(), refusedWith(err))
	}
}

// The whole thing, as Connect does it: the gate handshake, then the test,
// on a real socket.
func TestTestEndToEnd(t *testing.T) {
	f := newTestFixture(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	served := make(chan pretest.Report, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		a, err := f.verifier().Accept(conn)
		if err != nil {
			conn.Close()
			served <- pretest.Report{}
			return
		}
		rep, _ := pretest.Serve(conn, a.Test.Limits, a.At)
		a.Test.Done(rep.Moved())
		served <- rep
	}()

	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err := Dial(conn, "test", f.testToken(t, nil)); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	res, err := pretest.Run(ctx, conn, pretest.OptionsFor(leasetoken.DefaultTestLimits()))
	if err != nil {
		t.Fatal(err)
	}
	rep := <-served
	if !rep.Complete || len(res.RTTs) != leasetoken.TestPings || res.DownReceived != leasetoken.TestMaxDownBytes ||
		res.UpReceived != leasetoken.TestMaxUpBytes {
		t.Errorf("result %+v, report %+v", res, rep)
	}
	if left := f.guard.Remaining(); left != leasetoken.NodeHourlyTestBytes-rep.Moved() {
		t.Errorf("budget %d left after moving %d", left, rep.Moved())
	}
}
