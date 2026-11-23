package pretest

import (
	"context"
	"io"
	"net"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nvtohq/node/leasetoken"
)

// shaped carries src to dst as one direction of a line does: a bottleneck
// that sends bps bits a second (0: as fast as it comes) out of a bounded
// queue, and then oneWay of propagation delay. When the queue is full the
// proxy stops reading, so the sender's own socket buffers fill, as they
// would behind a real bottleneck. The sockets keep the OS's default
// buffers.
func shaped(src, dst net.Conn, oneWay time.Duration, bps float64) {
	type chunk struct {
		at time.Time // when it left the bottleneck
		b  []byte
	}
	// A router queue of about 100 ms at the line's rate, at least 64 KB.
	queueMax := max(int64(bps/8*0.1), 64<<10)
	var queued atomic.Int64
	queue := make(chan []byte, 1<<14)
	wire := make(chan chunk, 1<<14)

	go func() { // into the queue
		defer close(queue)
		for {
			for queued.Load() >= queueMax {
				time.Sleep(200 * time.Microsecond)
			}
			b := make([]byte, 16<<10)
			n, err := src.Read(b)
			if n > 0 {
				queued.Add(int64(n))
				queue <- b[:n]
			}
			if err != nil {
				return
			}
		}
	}()
	go func() { // the bottleneck
		defer close(wire)
		var free time.Time // when the line can next start a byte
		for b := range queue {
			if bps > 0 {
				if now := time.Now(); free.Before(now) {
					free = now
				}
				free = free.Add(time.Duration(float64(len(b)) * 8 / bps * float64(time.Second)))
				// Sleep only once a millisecond is owed: a sleep per small
				// chunk overshoots by tens of microseconds on Linux, which at
				// 200 Mbit/s halves the rate. The average rate is the same.
				if d := time.Until(free); d > time.Millisecond {
					time.Sleep(d)
				}
			}
			queued.Add(-int64(len(b)))
			wire <- chunk{time.Now(), b}
		}
	}()
	for c := range wire { // the far end
		time.Sleep(time.Until(c.at.Add(oneWay)))
		if _, err := dst.Write(c.b); err != nil {
			break
		}
	}
	go io.Copy(io.Discard, src)
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

// Over lines from fast and near to slow and far, the whole test ends
// inside the node's 10 s, with figures that match the line.
func TestShapedLinks(t *testing.T) {
	for _, c := range []struct {
		name     string
		rtt      time.Duration
		down, up float64 // bits a second; 0 for no limit
	}{
		{"300ms", 300 * time.Millisecond, 0, 0},
		{"300ms 4/2", 300 * time.Millisecond, 4e6, 2e6},
		{"150ms 4/2", 150 * time.Millisecond, 4e6, 2e6},
		{"150ms 20/10", 150 * time.Millisecond, 20e6, 10e6},
		{"100ms 10/5", 100 * time.Millisecond, 10e6, 5e6},
		{"50ms 50/20", 50 * time.Millisecond, 50e6, 20e6},
		{"10ms 200/100", 10 * time.Millisecond, 200e6, 100e6},
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
			t.Logf("%6s: %2d pings, rtt min %s, down %6.2f Mbit/s (%8d B), up %6.2f Mbit/s (%8d B)",
				took.Round(10*time.Millisecond), len(res.RTTs), res.RTTMin().Round(time.Millisecond),
				res.DownMbps(), res.DownReceived, res.UpMbps(), res.UpReceived)
			if err != nil || s.err != nil || !s.rep.Complete {
				t.Fatalf("after %s: client %v, server %v", took, err, s.err)
			}
			if took > 9500*time.Millisecond {
				t.Errorf("took %s, too close to the 10 s deadline", took)
			}
			if n := len(res.RTTs); n < 1 || n > lim.Pings {
				t.Errorf("%d pings", n)
			}
			if m := res.RTTMin(); m < c.rtt || m > c.rtt+30*time.Millisecond {
				t.Errorf("RTT min %s over a %s line", m, c.rtt)
			}
			sane := func(name string, got, line float64) {
				// On a busy CI runner, with the other cases running beside it,
				// the test's own shaper can fall far under its rate (116 of
				// 200 and 23 of 50 Mbit/s seen), so there only measuring more
				// than the line carries counts as wrong.
				low := 0.6
				if os.Getenv("CI") != "" {
					low = 0
				}
				switch {
				case got <= 0:
					t.Errorf("%s: nothing measured", name)
				case line > 0 && (got < line*low || got > line*1.15):
					t.Errorf("%s %.2f Mbit/s over a %.2f Mbit/s line", name, got/1e6, line/1e6)
				}
			}
			sane("down", res.DownMbps()*1e6, c.down)
			sane("up", res.UpMbps()*1e6, c.up)
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
