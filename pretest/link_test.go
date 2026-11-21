package pretest

import (
	"context"
	"io"
	"net"
	"testing"
	"time"

	"github.com/nodeexhq/miner/leasetoken"
)

// shaped forwards src to dst as a line would: each chunk arrives oneWay
// after it was sent, at most bps bits a second (0: unlimited), with a
// bounded amount in flight as a real path's buffers would hold.
func shaped(src, dst net.Conn, oneWay time.Duration, bps float64) {
	type chunk struct {
		at time.Time
		b  []byte
	}
	q := make(chan chunk, 16) // ≤ 256 KB in flight, on top of the sockets
	go func() {
		defer close(q)
		for {
			b := make([]byte, 16<<10)
			n, err := src.Read(b)
			if n > 0 {
				q <- chunk{time.Now(), b[:n]}
			}
			if err != nil {
				return
			}
		}
	}()
	var free time.Time // when the line is next free to carry a byte
	for c := range q {
		time.Sleep(time.Until(c.at.Add(oneWay)))
		if bps > 0 {
			if now := time.Now(); free.Before(now) {
				free = now
			}
			free = free.Add(time.Duration(float64(len(c.b)) * 8 / bps * float64(time.Second)))
			time.Sleep(time.Until(free))
		}
		if _, err := dst.Write(c.b); err != nil {
			break
		}
	}
	io.Copy(io.Discard, src)
	if tc, ok := dst.(*net.TCPConn); ok {
		tc.CloseWrite()
	}
}

// link is a client and server connected over a shaped path: rtt round
// trip, downBps from server to client and upBps back.
func link(t *testing.T, rtt time.Duration, downBps, upBps float64) (client, server net.Conn) {
	client, a := pair(t)
	b, server := pair(t)
	go shaped(a, b, rtt/2, upBps)
	go shaped(b, a, rtt/2, downBps)
	return client, server
}

// Over a line with a real round trip the test ends inside the Miner's
// deadline, with what it managed to measure. (At 300 ms, or on a slow line
// whose buffers hold seconds of data, the fixed windows alone leave too
// little of the 10 seconds; see PingWindow.)
func TestSlowLongLink(t *testing.T) {
	for _, c := range []struct {
		name     string
		rtt      time.Duration
		down, up float64
	}{
		{"150ms", 150 * time.Millisecond, 0, 0},
		{"50ms, 50/20 Mbit/s", 50 * time.Millisecond, 50e6, 20e6},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			client, server := link(t, c.rtt, c.down, c.up)
			lim := leasetoken.DefaultTestLimits()
			start := time.Now()
			done := serve(server, lim)
			res, err := Run(context.Background(), client, OptionsFor(lim))
			took := time.Since(start)
			s := <-done
			if err != nil || s.err != nil || !s.rep.Complete {
				t.Fatalf("after %s: client %v, server %v", took, err, s.err)
			}
			if took > 10*time.Second {
				t.Errorf("took %s", took)
			}
			if n := len(res.RTTs); n < 1 || n > lim.Pings {
				t.Errorf("%d pings", n)
			}
			if m := res.RTTMin(); m < c.rtt || m > c.rtt+100*time.Millisecond {
				t.Errorf("RTT min %s over a %s link", m, c.rtt)
			}
			within := func(name string, got, want float64) {
				if want > 0 && (got < want*0.5 || got > want*1.3) {
					t.Errorf("%s %.2f Mbit/s over a %.2f Mbit/s link", name, got/1e6, want/1e6)
				}
			}
			within("down", res.DownMbps()*1e6, c.down)
			within("up", res.UpMbps()*1e6, c.up)
			t.Logf("%s: %d pings, rtt %s, down %.2f Mbit/s (%d B), up %.2f Mbit/s (%d B)",
				took.Round(time.Millisecond), len(res.RTTs), res.RTTMedian().Round(time.Millisecond),
				res.DownMbps(), res.DownReceived, res.UpMbps(), res.UpReceived)
		})
	}
}

// Fewer pings than the token allows are fine; more are not (see
// TestClientOverLimits), and DOWN needs at least one.
func TestFewerPings(t *testing.T) {
	client, server := pair(t)
	done := serve(server, leasetoken.DefaultTestLimits())
	res, err := Run(context.Background(), client, Options{Pings: 1, DownBytes: 1000, UpBytes: 1000})
	if s := <-done; err != nil || s.err != nil || len(res.RTTs) != 1 || s.rep.Pings != 1 {
		t.Errorf("one ping: %d RTTs, %v / %+v %v", len(res.RTTs), err, s.rep, s.err)
	}
}
