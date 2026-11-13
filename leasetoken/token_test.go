package leasetoken

import (
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestIssueVerify(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	otherPub, otherPriv, _ := ed25519.GenerateKey(rand.Reader)
	now := time.Unix(1_800_000_000, 0)
	claims := Claims{LeaseID: "lease-1", NodeID: "node-1", Services: []string{ServiceSSH}, IssuedAt: now.Unix(), ExpiresAt: now.Add(TTL).Unix()}
	tok, err := Issue(priv, claims)
	if err != nil {
		t.Fatal(err)
	}

	got, err := Verify(pub, tok, "node-1", ServiceSSH, now)
	if err != nil || got.LeaseID != "lease-1" {
		t.Fatalf("valid token: %+v, %v", got, err)
	}

	forged, _ := Issue(otherPriv, claims)
	payload, sig, _ := strings.Cut(tok, ".")
	otherPayload, _, _ := strings.Cut(mustIssue(t, priv, Claims{LeaseID: "lease-2", NodeID: "node-1", Services: []string{ServiceSSH, ServiceVNC}, ExpiresAt: now.Add(TTL).Unix()}), ".")

	cases := []struct {
		name    string
		pub     ed25519.PublicKey
		token   string
		node    string
		service string
		at      time.Time
		want    error
	}{
		{"other platform key", otherPub, tok, "node-1", ServiceSSH, now, ErrSignature},
		{"signed by someone else", pub, forged, "node-1", ServiceSSH, now, ErrSignature},
		{"payload swapped under old signature", pub, otherPayload + "." + sig, "node-1", ServiceSSH, now, ErrSignature},
		{"expired", pub, tok, "node-1", ServiceSSH, now.Add(TTL), ErrExpired},
		{"other node", pub, tok, "node-2", ServiceSSH, now, ErrWrongNode},
		{"service not granted", pub, tok, "node-1", ServiceVNC, now, ErrService},
		{"no dot", pub, "garbage", "node-1", ServiceSSH, now, ErrMalformed},
		{"bad signature encoding", pub, payload + ".!!!", "node-1", ServiceSSH, now, ErrMalformed},
		{"empty", pub, "", "node-1", ServiceSSH, now, ErrMalformed},
	}
	for _, c := range cases {
		if _, err := Verify(c.pub, c.token, c.node, c.service, c.at); !errors.Is(err, c.want) {
			t.Errorf("%s: got %v, want %v", c.name, err, c.want)
		}
	}
}

func mustIssue(t *testing.T, priv ed25519.PrivateKey, c Claims) string {
	t.Helper()
	tok, err := Issue(priv, c)
	if err != nil {
		t.Fatal(err)
	}
	return tok
}
