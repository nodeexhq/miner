// Package leasetoken issues and verifies the short-lived tokens that let a
// renter's Connect client open connections on a leased node. The platform
// signs them; Miners verify them offline against the platform's public key.
package leasetoken

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"time"
)

// TTL is deliberately short: Connect refreshes tokens while a lease is
// active, so a leaked token is useful for minutes at most.
const TTL = 10 * time.Minute

const prefix = "NODEEX-LEASE-V1."

// Services a token may grant.
const (
	ServiceSSH = "ssh"
	ServiceVNC = "vnc"
)

type Claims struct {
	LeaseID   string   `json:"lease_id"`
	NodeID    string   `json:"node_id"`
	Services  []string `json:"services"`
	IssuedAt  int64    `json:"iat"`
	ExpiresAt int64    `json:"exp"`
}

var (
	ErrMalformed = errors.New("leasetoken: malformed token")
	ErrSignature = errors.New("leasetoken: signature does not verify")
	ErrExpired   = errors.New("leasetoken: token expired")
	ErrWrongNode = errors.New("leasetoken: token was issued for a different node")
	ErrService   = errors.New("leasetoken: token does not grant this service")
)

var enc = base64.RawURLEncoding

// Issue signs claims and returns the token string.
func Issue(priv ed25519.PrivateKey, c Claims) (string, error) {
	payload, err := json.Marshal(c)
	if err != nil {
		return "", err
	}
	p := enc.EncodeToString(payload)
	sig := ed25519.Sign(priv, []byte(prefix+p))
	return p + "." + enc.EncodeToString(sig), nil
}

// Verify checks the token's signature and expiry, and that it grants service
// on nodeID. Callers must additionally check Claims.LeaseID against the
// lease they currently hold.
func Verify(pub ed25519.PublicKey, token, nodeID, service string, now time.Time) (Claims, error) {
	var c Claims
	p, s, ok := strings.Cut(token, ".")
	if !ok {
		return c, ErrMalformed
	}
	sig, err := enc.DecodeString(s)
	if err != nil || len(sig) != ed25519.SignatureSize {
		return c, ErrMalformed
	}
	if len(pub) != ed25519.PublicKeySize || !ed25519.Verify(pub, []byte(prefix+p), sig) {
		return c, ErrSignature
	}
	payload, err := enc.DecodeString(p)
	if err != nil || json.Unmarshal(payload, &c) != nil {
		return c, ErrMalformed
	}
	if now.Unix() >= c.ExpiresAt {
		return c, ErrExpired
	}
	if c.NodeID != nodeID {
		return c, ErrWrongNode
	}
	if !slices.Contains(c.Services, service) {
		return c, ErrService
	}
	return c, nil
}
