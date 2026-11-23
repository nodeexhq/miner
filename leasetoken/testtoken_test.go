package leasetoken

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

func testGrant(now time.Time) TestGrant {
	return TestGrant{
		NodeID: "node-1", ReservationID: "res-1", AccountID: "acct-1", TokenID: NewTokenID(),
		IssuedAt: now, ExpiresAt: now.Add(TestTTL), Limits: DefaultTestLimits(),
	}
}

func TestTestTokenRoundTrip(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	now := time.Unix(1_800_000_000, 0)
	g := testGrant(now)
	tok, issued, err := IssueTest(priv, g)
	if err != nil {
		t.Fatal(err)
	}
	c, err := VerifyTest(pub, tok, "node-1", now.Add(time.Minute))
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if c.TokenID != g.TokenID || c.ReservationID != "res-1" || c.Scope != ScopeTest || c.LeaseID != "" ||
		c.Test == nil || *c.Test != DefaultTestLimits() || c.Account != AccountTag("acct-1", "res-1") || c.Account == "acct-1" {
		t.Errorf("claims = %+v, issued %+v", c, issued)
	}

	// The exact wire format the node parses.
	payload, _, _ := strings.Cut(tok, ".")
	raw, _ := base64.RawURLEncoding.DecodeString(payload)
	var m map[string]any
	json.Unmarshal(raw, &m)
	for _, k := range []string{"lease_id", "node_id", "services", "iat", "exp", "scope", "jti", "reservation_id", "acct", "test"} {
		if _, ok := m[k]; !ok {
			t.Errorf("claim %q missing from %s", k, raw)
		}
	}
	if len(c.TokenID) != 32 {
		t.Errorf("token id %q is not 128 bits of hex", c.TokenID)
	}

	// A test token is no use for anything else, and a lease token is no
	// use for a test: a node that only knows lease tokens refuses a test
	// token for ssh or vnc (it grants neither), and for "test" it names no
	// lease, so the gate's current-lease check refuses it too.
	for _, svc := range []string{ServiceSSH, ServiceVNC} {
		if _, err := Verify(pub, tok, "node-1", svc, now); !errors.Is(err, ErrService) {
			t.Errorf("test token for %s: %v", svc, err)
		}
	}
	if c, err := Verify(pub, tok, "node-1", ServiceTest, now); err != nil || c.LeaseID != "" {
		t.Errorf("Verify of a test token for the test service: %+v %v", c, err)
	}
	lease, _ := Issue(priv, Claims{LeaseID: "lease-1", NodeID: "node-1", Services: []string{ServiceSSH, ServiceVNC, ServiceTest},
		IssuedAt: now.Unix(), ExpiresAt: now.Add(TTL).Unix()})
	if _, err := VerifyTest(pub, lease, "node-1", now); !errors.Is(err, ErrScope) {
		t.Errorf("lease token as a test token: %v", err)
	}
	if _, err := Verify(pub, lease, "node-1", ServiceTest, now); !errors.Is(err, ErrScope) {
		t.Errorf("lease token for the test service: %v", err)
	}

	for name, c := range map[string]struct {
		node string
		at   time.Time
		pub  ed25519.PublicKey
		want error
	}{
		"other node": {"node-2", now, pub, ErrWrongNode},
		"expired":    {"node-1", now.Add(TestTTL), pub, ErrExpired},
		"other key":  {"node-1", now, func() ed25519.PublicKey { p, _, _ := ed25519.GenerateKey(rand.Reader); return p }(), ErrSignature},
	} {
		if _, err := VerifyTest(c.pub, tok, c.node, c.at); !errors.Is(err, c.want) {
			t.Errorf("%s: %v, want %v", name, err, c.want)
		}
	}

	// The platform never issues one that lives too long or is incomplete.
	long := testGrant(now)
	long.ExpiresAt = now.Add(TestTTL + time.Second)
	if _, _, err := IssueTest(priv, long); !errors.Is(err, ErrTestGrant) {
		t.Errorf("token for longer than TestTTL: %v", err)
	}
	noID := testGrant(now)
	noID.TokenID = ""
	if _, _, err := IssueTest(priv, noID); !errors.Is(err, ErrTestGrant) {
		t.Errorf("token without an id: %v", err)
	}
	// Nor would a node take one: a hand-made test token without a jti.
	bare, _ := Issue(priv, Claims{NodeID: "node-1", Services: []string{ServiceTest}, Scope: ScopeTest, ReservationID: "res-1",
		Test: &TestLimits{}, IssuedAt: now.Unix(), ExpiresAt: now.Add(time.Minute).Unix()})
	if _, err := VerifyTest(pub, bare, "node-1", now); !errors.Is(err, ErrMalformed) {
		t.Errorf("test token without jti: %v", err)
	}
}

// The node's single-use rule: a token id is accepted once, and refused for
// as long as the token could verify.
func TestReplayGuard(t *testing.T) {
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	now := time.Unix(1_800_000_000, 0)
	var g ReplayGuard
	_, a, _ := IssueTest(priv, testGrant(now))
	_, b, _ := IssueTest(priv, testGrant(now))
	if !g.Use(a, now) {
		t.Fatal("first use refused")
	}
	if g.Use(a, now.Add(time.Minute)) {
		t.Error("second use of the same token id accepted")
	}
	if !g.Use(b, now.Add(time.Minute)) {
		t.Error("another token refused")
	}
	if g.Use(a, now.Add(TestTTL-time.Second)) {
		t.Error("token id forgotten before its token expired")
	}
	// Once the token has expired it is refused by verification anyway, so
	// the guard may forget it.
	g.Use(b, now.Add(TestTTL))
	if len(g.seen) != 0 && g.seen[a.TokenID] != 0 {
		t.Errorf("expired ids kept: %v", g.seen)
	}
}

func TestClamp(t *testing.T) {
	got := TestLimits{Pings: 1000, MaxDownBytes: 1 << 40, MaxUpBytes: -5, MaxSeconds: 60}.Clamp()
	want := TestLimits{NodeMaxPings, NodeMaxDownBytes, 0, NodeMaxSeconds}
	if got != want {
		t.Errorf("Clamp = %+v, want %+v", got, want)
	}
	if d := DefaultTestLimits(); d.Clamp() != d {
		t.Errorf("platform defaults exceed the node's maxima: %+v", d)
	}
}
