// Package gate is the handshake that guards every connection a Miner
// forwards into a leased machine. Connect opens a stream, sends one line
// naming the service and carrying a lease token, and the Miner answers
// before any payload flows:
//
//	C: NODEEX1 <service> <token>\n
//	S: OK\n            (or)  ERR <reason>\n
//
// The overlay network's ACL only decides who can reach the Miner at all;
// this handshake is what ties a connection to a paid, current lease.
//
// One service is not a lease's: "test", the connection test a renter runs
// before ordering (package pretest). It takes only a test token, which is
// good for nothing else, and only while no lease is active on the node.
package gate

import (
	"crypto/ed25519"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"time"

	"github.com/nodeexhq/miner/leasetoken"
	"github.com/nodeexhq/miner/pretest"
)

const (
	magic            = "NODEEX1"
	maxPreamble      = 2048
	handshakeTimeout = 10 * time.Second
)

var (
	ErrProtocol = errors.New("gate: malformed handshake")
	ErrNoLease  = errors.New("gate: token is not for the node's current lease")
	ErrNoTests  = errors.New("gate: this node does not run connection tests")
	ErrIssuedAt = errors.New("gate: test token issued in the future")
)

// issuedSkew is how far ahead of the Miner's clock a test token's issue
// time may be. Without a bound, a token issued "later" would outlive the
// five minutes a test token may live.
const issuedSkew = time.Minute

// Refused is returned by Dial when the Miner rejects the handshake.
type Refused struct{ Reason string }

func (e *Refused) Error() string { return "gate: refused: " + e.Reason }

// Dial performs the client side of the handshake on conn. On success conn is
// ready to carry the service's own protocol.
func Dial(conn net.Conn, service, token string) error {
	conn.SetDeadline(time.Now().Add(handshakeTimeout))
	defer conn.SetDeadline(time.Time{})
	if _, err := fmt.Fprintf(conn, "%s %s %s\n", magic, service, token); err != nil {
		return err
	}
	// Read byte-wise so nothing past the reply line is consumed: the bytes
	// after it belong to the tunnelled protocol.
	line, err := readLine(conn, 256)
	if err != nil {
		return err
	}
	if line == "OK" {
		return nil
	}
	if reason, ok := strings.CutPrefix(line, "ERR "); ok {
		return &Refused{reason}
	}
	return ErrProtocol
}

// Verifier is the Miner's view of what it may currently serve.
type Verifier struct {
	PlatformKey ed25519.PublicKey
	NodeID      string
	// CurrentLease returns the ID of the lease the platform says is active
	// on this node, or "" when there is none.
	CurrentLease func() string
	Now          func() time.Time
	// Tests admits connection tests; nil refuses the test service.
	Tests *pretest.Guard
}

// Admitted is a connection the gate let through.
type Admitted struct {
	Service string
	Claims  leasetoken.Claims
	// Test is the admitted test, for the test service. The caller must
	// end it with Done.
	Test *pretest.Ticket
	// At is when the gate sent OK: a test's time runs from here.
	At time.Time
}

// Accept performs the Miner side of the handshake. It returns what it
// admitted, or an error after telling the client it was refused.
func (v Verifier) Accept(conn net.Conn) (Admitted, error) {
	conn.SetDeadline(time.Now().Add(handshakeTimeout))
	defer conn.SetDeadline(time.Time{})

	a, err := v.check(conn)
	if err != nil {
		// The reason sent to the client is deliberately coarse.
		fmt.Fprintf(conn, "ERR %s\n", publicReason(err))
		return Admitted{Claims: a.Claims}, err
	}
	if _, err := io.WriteString(conn, "OK\n"); err != nil {
		if a.Test != nil {
			a.Test.Done(0)
		}
		return Admitted{Claims: a.Claims}, err
	}
	a.At = time.Now()
	return a, nil
}

func (v Verifier) check(conn net.Conn) (Admitted, error) {
	var a Admitted
	line, err := readLine(conn, maxPreamble)
	if err != nil {
		return a, ErrProtocol
	}
	parts := strings.Split(line, " ")
	if len(parts) != 3 || parts[0] != magic {
		return a, ErrProtocol
	}
	service, token := parts[1], parts[2]
	now := time.Now
	if v.Now != nil {
		now = v.Now
	}
	if service == leasetoken.ServiceTest {
		return v.checkTest(token, now())
	}
	// Verify refuses a test token for any other service.
	a.Claims, err = leasetoken.Verify(v.PlatformKey, token, v.NodeID, service, now())
	if err != nil {
		return a, err
	}
	if cur := v.CurrentLease(); cur == "" || cur != a.Claims.LeaseID {
		return a, ErrNoLease
	}
	a.Service = service
	return a, nil
}

// checkTest admits a connection test: a well-formed test token for this
// node, never used before, while the node is free for one.
func (v Verifier) checkTest(token string, now time.Time) (Admitted, error) {
	a := Admitted{Service: leasetoken.ServiceTest}
	var err error
	if v.Tests == nil {
		return a, ErrNoTests
	}
	// VerifyTest refuses a lease token, and checks services == ["test"],
	// an id, a reservation, no lease, and exp - iat <= TestTTL.
	if a.Claims, err = leasetoken.VerifyTest(v.PlatformKey, token, v.NodeID, now); err != nil {
		return a, err
	}
	if a.Claims.IssuedAt > now.Add(issuedSkew).Unix() {
		return a, ErrIssuedAt
	}
	a.Test, err = v.Tests.Admit(a.Claims)
	return a, err
}

func publicReason(err error) string {
	switch {
	case errors.Is(err, ErrProtocol):
		return "protocol"
	case errors.Is(err, leasetoken.ErrExpired):
		return "token-expired"
	case errors.Is(err, ErrNoLease):
		return "no-active-lease"
	case errors.Is(err, pretest.ErrTokenUsed):
		return "token-used"
	case errors.Is(err, pretest.ErrBusy):
		return "busy"
	default:
		return "unauthorized"
	}
}

// readLine reads up to and including '\n' one byte at a time, so it never
// buffers past the line. It fails if the line exceeds max bytes.
func readLine(r io.Reader, max int) (string, error) {
	var sb strings.Builder
	b := make([]byte, 1)
	for sb.Len() < max {
		if _, err := io.ReadFull(r, b); err != nil {
			return "", ErrProtocol
		}
		if b[0] == '\n' {
			return sb.String(), nil
		}
		sb.WriteByte(b[0])
	}
	return "", ErrProtocol
}
