// Package devicesig signs and verifies Miner requests with the node's Ed25519
// device key.
package devicesig

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"strconv"
	"time"
)

const (
	HeaderTimestamp = "X-NodeEx-Timestamp"
	HeaderSignature = "X-NodeEx-Signature"

	// MaxSkew bounds how far a request timestamp may be from server time,
	// which also bounds the replay window.
	MaxSkew = 5 * time.Minute
)

var (
	ErrMalformed = errors.New("devicesig: malformed signature headers")
	ErrSkew      = errors.New("devicesig: timestamp outside allowed window")
	ErrInvalid   = errors.New("devicesig: signature does not verify")
)

func message(method, path, ts string, body []byte) []byte {
	sum := sha256.Sum256(body)
	return []byte("NODEEX-V1\n" + method + "\n" + path + "\n" + ts + "\n" + hex.EncodeToString(sum[:]))
}

// Sign returns the timestamp and signature header values for a request.
func Sign(priv ed25519.PrivateKey, method, path string, body []byte, now time.Time) (ts, sig string) {
	ts = strconv.FormatInt(now.Unix(), 10)
	sig = base64.StdEncoding.EncodeToString(ed25519.Sign(priv, message(method, path, ts, body)))
	return ts, sig
}

// Verify checks the signature headers of a request against pub.
func Verify(pub ed25519.PublicKey, method, path string, body []byte, ts, sig string, now time.Time) error {
	if len(pub) != ed25519.PublicKeySize {
		return ErrInvalid
	}
	sec, err := strconv.ParseInt(ts, 10, 64)
	if err != nil {
		return ErrMalformed
	}
	raw, err := base64.StdEncoding.DecodeString(sig)
	if err != nil || len(raw) != ed25519.SignatureSize {
		return ErrMalformed
	}
	if d := now.Sub(time.Unix(sec, 0)); d > MaxSkew || d < -MaxSkew {
		return ErrSkew
	}
	if !ed25519.Verify(pub, message(method, path, ts, body), raw) {
		return ErrInvalid
	}
	return nil
}
