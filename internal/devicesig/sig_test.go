package devicesig

import (
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"testing"
	"time"
)

func TestSignVerify(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	other, _, _ := ed25519.GenerateKey(rand.Reader)
	now := time.Unix(1_800_000_000, 0)
	body := []byte(`{"a":1}`)
	ts, sig := Sign(priv, "POST", "/v1/nodes/x/heartbeat", body, now)

	cases := []struct {
		name               string
		pub                ed25519.PublicKey
		method, path, body string
		ts, sig            string
		now                time.Time
		want               error
	}{
		{"ok", pub, "POST", "/v1/nodes/x/heartbeat", string(body), ts, sig, now, nil},
		{"ok within skew", pub, "POST", "/v1/nodes/x/heartbeat", string(body), ts, sig, now.Add(4 * time.Minute), nil},
		{"wrong key", other, "POST", "/v1/nodes/x/heartbeat", string(body), ts, sig, now, ErrInvalid},
		{"tampered body", pub, "POST", "/v1/nodes/x/heartbeat", `{"a":2}`, ts, sig, now, ErrInvalid},
		{"other path", pub, "POST", "/v1/nodes/y/heartbeat", string(body), ts, sig, now, ErrInvalid},
		{"other method", pub, "PUT", "/v1/nodes/x/heartbeat", string(body), ts, sig, now, ErrInvalid},
		{"stale", pub, "POST", "/v1/nodes/x/heartbeat", string(body), ts, sig, now.Add(6 * time.Minute), ErrSkew},
		{"from the future", pub, "POST", "/v1/nodes/x/heartbeat", string(body), ts, sig, now.Add(-6 * time.Minute), ErrSkew},
		{"bad ts", pub, "POST", "/v1/nodes/x/heartbeat", string(body), "soon", sig, now, ErrMalformed},
		{"bad sig", pub, "POST", "/v1/nodes/x/heartbeat", string(body), ts, "!!", now, ErrMalformed},
		{"short key", pub[:5], "POST", "/v1/nodes/x/heartbeat", string(body), ts, sig, now, ErrInvalid},
	}
	for _, c := range cases {
		err := Verify(c.pub, c.method, c.path, []byte(c.body), c.ts, c.sig, c.now)
		if !errors.Is(err, c.want) {
			t.Errorf("%s: got %v, want %v", c.name, err, c.want)
		}
	}
}
