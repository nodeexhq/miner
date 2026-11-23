package leasetoken

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"sync"
	"time"
)

// Test tokens admit a renter's Connect client to a node's connection test,
// before any lease exists. They are lease tokens in format and signature
// (same key, same "NVTO-LEASE-V1." signing prefix, so nodes verify them
// offline exactly as they verify lease tokens) with these differences:
//
//   - "services" is exactly ["test"] and "lease_id" is empty, so a node
//     that knows nothing of tests refuses them: they grant no lease service,
//     and they name no lease;
//   - "scope" is "test", and the node only takes a token with that scope
//     for the test service, and never one without it;
//   - "jti" is a random token id. The token is single use: a node that has
//     accepted a token id must refuse it again until the token expires;
//   - "reservation_id" names the reservation the test is for, and "acct" is
//     an opaque tag of the renter's account (see AccountTag) for the
//     node's logs; neither is checked by the node;
//   - "test" carries the platform's caps for the test (TestLimits). The
//     node applies them, but never more than its own hard maxima;
//   - exp - iat is at most TestTTL.
const (
	ServiceTest = "test"
	ScopeTest   = "test"

	// TestTTL is the longest a test token lives: never longer than the
	// reservation it belongs to.
	TestTTL = 5 * time.Minute
)

// The platform's caps for one test, put in every test token.
const (
	TestPings        = 20
	TestMaxDownBytes = 10_000_000 // 10 MB, node to Connect
	TestMaxUpBytes   = 10_000_000 // 10 MB, Connect to node
	TestMaxSeconds   = 10
)

// Hard maxima the node keeps whatever a token says, and its own budget for
// all tests together.
const (
	NodeMaxPings        = 50
	NodeMaxDownBytes    = 10_000_000
	NodeMaxUpBytes      = 10_000_000
	NodeMaxSeconds      = 10
	NodeHourlyTestBytes = 200_000_000 // both directions, all tests, rolling hour
)

// TestLimits are the caps of one test.
type TestLimits struct {
	Pings        int   `json:"pings"`
	MaxDownBytes int64 `json:"max_down_bytes"`
	MaxUpBytes   int64 `json:"max_up_bytes"`
	MaxSeconds   int   `json:"max_seconds"`
}

// DefaultTestLimits are the platform's caps.
func DefaultTestLimits() TestLimits {
	return TestLimits{TestPings, TestMaxDownBytes, TestMaxUpBytes, TestMaxSeconds}
}

// Clamp returns the limits a node applies: the token's, but never above its
// hard maxima.
func (l TestLimits) Clamp() TestLimits {
	return TestLimits{
		Pings:        min(max(l.Pings, 0), NodeMaxPings),
		MaxDownBytes: min(max(l.MaxDownBytes, 0), NodeMaxDownBytes),
		MaxUpBytes:   min(max(l.MaxUpBytes, 0), NodeMaxUpBytes),
		MaxSeconds:   min(max(l.MaxSeconds, 0), NodeMaxSeconds),
	}
}

// TestGrant is what a test token is issued for.
type TestGrant struct {
	NodeID        string
	ReservationID string
	AccountID     string
	TokenID       string
	IssuedAt      time.Time
	ExpiresAt     time.Time
	Limits        TestLimits
}

var ErrTestGrant = errors.New("leasetoken: test grant is incomplete or lives too long")

// NewTokenID returns a random token id: 128 bits, hex.
func NewTokenID() string {
	b := make([]byte, 16)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// AccountTag is the opaque "acct" claim: it ties a token to the renter's
// account and reservation without telling the node's owner who the renter
// is.
func AccountTag(accountID, reservationID string) string {
	sum := sha256.Sum256([]byte("nvto-test-acct\x00" + accountID + "\x00" + reservationID))
	return hex.EncodeToString(sum[:16])
}

// IssueTest signs a test token.
func IssueTest(priv ed25519.PrivateKey, g TestGrant) (string, Claims, error) {
	c := Claims{
		NodeID: g.NodeID, Services: []string{ServiceTest},
		IssuedAt: g.IssuedAt.Unix(), ExpiresAt: g.ExpiresAt.Unix(),
		Scope: ScopeTest, TokenID: g.TokenID, ReservationID: g.ReservationID,
		Account: AccountTag(g.AccountID, g.ReservationID), Test: &g.Limits,
	}
	if g.NodeID == "" || g.ReservationID == "" || g.TokenID == "" ||
		c.ExpiresAt <= c.IssuedAt || c.ExpiresAt-c.IssuedAt > int64(TestTTL/time.Second) {
		return "", c, ErrTestGrant
	}
	tok, err := Issue(priv, c)
	return tok, c, err
}

// VerifyTest is what the node's gate does with a token presented for the
// test service: the signature, expiry and node as for any token, then that
// it is a well-formed test token. It does not check single use; see
// ReplayGuard.
func VerifyTest(pub ed25519.PublicKey, token, nodeID string, now time.Time) (Claims, error) {
	c, err := verify(pub, token, nodeID, ServiceTest, now)
	if err != nil {
		return c, err
	}
	if c.Scope != ScopeTest {
		return c, ErrScope
	}
	if c.TokenID == "" || c.ReservationID == "" || c.LeaseID != "" || c.Test == nil ||
		len(c.Services) != 1 || c.ExpiresAt-c.IssuedAt > int64(TestTTL/time.Second) {
		return c, ErrMalformed
	}
	return c, nil
}

// ReplayGuard remembers the test token ids a node has accepted, until the
// tokens expire. It is the reference for the node's single-use rule.
type ReplayGuard struct {
	mu   sync.Mutex
	seen map[string]int64 // token id -> exp (Unix seconds)
}

// Use reports whether the token id may be used now, and if so marks it used.
// A token id is refused for as long as the token could still verify.
func (g *ReplayGuard) Use(c Claims, now time.Time) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.seen == nil {
		g.seen = map[string]int64{}
	}
	for id, exp := range g.seen {
		if now.Unix() >= exp {
			delete(g.seen, id)
		}
	}
	if _, used := g.seen[c.TokenID]; used {
		return false
	}
	g.seen[c.TokenID] = c.ExpiresAt
	return true
}
