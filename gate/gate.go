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
)

const (
	magic            = "NODEEX1"
	maxPreamble      = 2048
	handshakeTimeout = 10 * time.Second
)

var (
	ErrProtocol = errors.New("gate: malformed handshake")
	ErrNoLease  = errors.New("gate: token is not for the node's current lease")
)

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
}

// Accept performs the Miner side of the handshake. It returns the requested
// service and the verified claims, or an error after telling the client it
// was refused.
func (v Verifier) Accept(conn net.Conn) (string, leasetoken.Claims, error) {
	conn.SetDeadline(time.Now().Add(handshakeTimeout))
	defer conn.SetDeadline(time.Time{})

	service, claims, err := v.check(conn)
	if err != nil {
		// The reason sent to the client is deliberately coarse.
		fmt.Fprintf(conn, "ERR %s\n", publicReason(err))
		return "", claims, err
	}
	if _, err := io.WriteString(conn, "OK\n"); err != nil {
		return "", claims, err
	}
	return service, claims, nil
}

func (v Verifier) check(conn net.Conn) (string, leasetoken.Claims, error) {
	var none leasetoken.Claims
	line, err := readLine(conn, maxPreamble)
	if err != nil {
		return "", none, ErrProtocol
	}
	parts := strings.Split(line, " ")
	if len(parts) != 3 || parts[0] != magic {
		return "", none, ErrProtocol
	}
	service, token := parts[1], parts[2]
	now := time.Now
	if v.Now != nil {
		now = v.Now
	}
	claims, err := leasetoken.Verify(v.PlatformKey, token, v.NodeID, service, now())
	if err != nil {
		return "", claims, err
	}
	if cur := v.CurrentLease(); cur == "" || cur != claims.LeaseID {
		return "", claims, ErrNoLease
	}
	return service, claims, nil
}

func publicReason(err error) string {
	switch {
	case errors.Is(err, ErrProtocol):
		return "protocol"
	case errors.Is(err, leasetoken.ErrExpired):
		return "token-expired"
	case errors.Is(err, ErrNoLease):
		return "no-active-lease"
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
