package gate

import (
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/nodeexhq/miner/leasetoken"
)

type fixture struct {
	pub   ed25519.PublicKey
	priv  ed25519.PrivateKey
	now   time.Time
	lease string
}

func newFixture() *fixture {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	return &fixture{pub: pub, priv: priv, now: time.Unix(1_800_000_000, 0), lease: "lease-1"}
}

func (f *fixture) token(t *testing.T, lease, node string, services ...string) string {
	t.Helper()
	tok, err := leasetoken.Issue(f.priv, leasetoken.Claims{
		LeaseID: lease, NodeID: node, Services: services,
		IssuedAt: f.now.Unix(), ExpiresAt: f.now.Add(leasetoken.TTL).Unix(),
	})
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

func (f *fixture) verifier() Verifier {
	return Verifier{
		PlatformKey:  f.pub,
		NodeID:       "node-1",
		CurrentLease: func() string { return f.lease },
		Now:          func() time.Time { return f.now },
	}
}

// handshake runs both sides over a pipe and returns each side's result.
func (f *fixture) handshake(service, token string) (acceptedService string, serverErr, clientErr error) {
	c, s := net.Pipe()
	defer c.Close()
	defer s.Close()
	done := make(chan struct{})
	go func() {
		defer close(done)
		acceptedService, _, serverErr = f.verifier().Accept(s)
	}()
	clientErr = Dial(c, service, token)
	<-done
	return
}

func TestHandshake(t *testing.T) {
	f := newFixture()
	other := newFixture()

	svc, serverErr, clientErr := f.handshake("ssh", f.token(t, "lease-1", "node-1", "ssh", "vnc"))
	if svc != "ssh" || serverErr != nil || clientErr != nil {
		t.Fatalf("valid handshake: %q, %v, %v", svc, serverErr, clientErr)
	}

	cases := []struct {
		name       string
		service    string
		token      string
		mutate     func()
		wantReason string
	}{
		{"token for another lease", "ssh", f.token(t, "lease-0", "node-1", "ssh"), nil, "no-active-lease"},
		{"token for another node", "ssh", f.token(t, "lease-1", "node-2", "ssh"), nil, "unauthorized"},
		{"service not granted", "vnc", f.token(t, "lease-1", "node-1", "ssh"), nil, "unauthorized"},
		{"signed by another key", "ssh", other.token(t, "lease-1", "node-1", "ssh"), nil, "unauthorized"},
		{"garbage token", "ssh", "garbage", nil, "unauthorized"},
		{"lease ended on the node", "ssh", f.token(t, "lease-1", "node-1", "ssh"), func() { f.lease = "" }, "no-active-lease"},
		{"expired token", "ssh", f.token(t, "lease-1", "node-1", "ssh"), func() { f.now = f.now.Add(leasetoken.TTL + time.Second) }, "token-expired"},
	}
	for _, c := range cases {
		saved := *f
		if c.mutate != nil {
			c.mutate()
		}
		_, serverErr, clientErr := f.handshake(c.service, c.token)
		var refused *Refused
		if serverErr == nil || !errors.As(clientErr, &refused) || refused.Reason != c.wantReason {
			t.Errorf("%s: server %v, client %v; want refusal %q", c.name, serverErr, clientErr, c.wantReason)
		}
		*f = saved
	}
}

func TestAcceptRejectsMalformedPreambles(t *testing.T) {
	f := newFixture()
	for name, preamble := range map[string]string{
		"wrong magic":   "HELLO ssh tok\n",
		"too few parts": "NODEEX1 ssh\n",
		"extra parts":   "NODEEX1 ssh tok extra\n",
		"ssh banner":    "SSH-2.0-OpenSSH_9.9\r\n",
		"oversized":     "NODEEX1 ssh " + strings.Repeat("a", maxPreamble) + "\n",
	} {
		c, s := net.Pipe()
		// A pipe is unbuffered, so read the reply concurrently with writing.
		go io.WriteString(c, preamble)
		go io.Copy(io.Discard, c)
		_, _, err := f.verifier().Accept(s)
		if !errors.Is(err, ErrProtocol) {
			t.Errorf("%s: got %v, want ErrProtocol", name, err)
		}
		c.Close()
		s.Close()
	}
}

// The handshake must not swallow bytes that follow it: they belong to the
// tunnelled protocol.
func TestHandshakeLeavesPayloadIntact(t *testing.T) {
	f := newFixture()
	c, s := net.Pipe()
	defer c.Close()
	defer s.Close()

	got := make(chan string, 1)
	go func() {
		if _, _, err := f.verifier().Accept(s); err != nil {
			got <- "accept: " + err.Error()
			return
		}
		io.WriteString(s, "server-banner\n")
		buf := make([]byte, len("client-hello"))
		io.ReadFull(s, buf)
		got <- string(buf)
	}()

	if err := Dial(c, "ssh", f.token(t, "lease-1", "node-1", "ssh")); err != nil {
		t.Fatal(err)
	}
	go io.WriteString(c, "client-hello")
	banner := make([]byte, len("server-banner\n"))
	if _, err := io.ReadFull(c, banner); err != nil || string(banner) != "server-banner\n" {
		t.Fatalf("client read %q, %v", banner, err)
	}
	if s := <-got; s != "client-hello" {
		t.Fatalf("server read %q", s)
	}
}
