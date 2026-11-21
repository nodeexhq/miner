package pretest

import (
	"context"
	crand "crypto/rand"
	"fmt"
	"math/rand/v2"
	"net"
	"slices"
	"time"

	"github.com/nodeexhq/miner/leasetoken"
)

// Options say how much of a test the client runs. Each must be within the
// test token's limits, or the Miner refuses the test part-way.
type Options struct {
	Pings     int // at most; the client stops after PingWindow, having sent at least one
	DownBytes int64
	UpBytes   int64
}

// OptionsFor is the whole test a token's limits allow.
func OptionsFor(l leasetoken.TestLimits) Options {
	l = l.Clamp()
	return Options{Pings: l.Pings, DownBytes: l.MaxDownBytes, UpBytes: l.MaxUpBytes}
}

// Result is what the client measured, and what the server reported.
type Result struct {
	// RTTs are the ping round trips, in the order sent.
	RTTs []time.Duration

	// Download, as received: bytes and the time from the first DATA
	// frame's arrival to the last one's, with Counted leaving out the first
	// frame (its arrival starts the clock).
	DownReceived int64
	DownCounted  int64
	DownElapsed  time.Duration
	// The server's DONE: what it sent, and over how long.
	DownServerSent    int64
	DownServerElapsed time.Duration

	// Upload: what the client sent, and the server's UPRESULT.
	UpSent     int64
	UpReceived int64
	UpCounted  int64
	UpElapsed  time.Duration
}

// RTTMin and RTTMedian summarise the pings; zero when there were none.
func (r Result) RTTMin() time.Duration {
	if len(r.RTTs) == 0 {
		return 0
	}
	return slices.Min(r.RTTs)
}

func (r Result) RTTMedian() time.Duration {
	if len(r.RTTs) == 0 {
		return 0
	}
	s := slices.Clone(r.RTTs)
	slices.Sort(s)
	return s[len(s)/2]
}

// DownMbps and UpMbps are the throughput each way in megabits a second,
// from the counted bytes; zero when too little was moved to tell.
func (r Result) DownMbps() float64 { return mbps(r.DownCounted, r.DownElapsed) }
func (r Result) UpMbps() float64   { return mbps(r.UpCounted, r.UpElapsed) }

func mbps(n int64, d time.Duration) float64 {
	if n <= 0 || d <= 0 {
		return 0
	}
	return float64(n) * 8 / d.Seconds() / 1e6
}

// Run performs the client's side of a test on conn, on which gate.Dial
// with service "test" has just succeeded. It returns what it measured; a
// refusal by the Miner is an *Error. ctx bounds the whole test (the Miner
// closes it after the token's max_seconds in any case). Run does not close
// conn, but conn is of no further use after it.
func Run(ctx context.Context, conn net.Conn, opt Options) (Result, error) {
	var res Result
	if opt.Pings < 0 || opt.DownBytes < 0 || opt.UpBytes < 0 ||
		opt.DownBytes > 1<<32-1 || opt.UpBytes > 1<<32-1 {
		return res, fmt.Errorf("pretest: invalid options %+v", opt)
	}
	if d, ok := ctx.Deadline(); ok {
		conn.SetDeadline(d)
	}
	stop := context.AfterFunc(ctx, func() { conn.SetDeadline(time.Unix(1, 0)) })
	c := &client{conn: conn, fr: startReader(conn)}
	err := c.run(opt, &res)
	stop()
	// End the reader, then leave the connection as it was.
	conn.SetReadDeadline(time.Unix(1, 0))
	c.fr.stop()
	conn.SetDeadline(time.Time{})
	if err != nil && ctx.Err() != nil {
		err = fmt.Errorf("pretest: %w (%w)", ctx.Err(), err)
	}
	return res, err
}

type client struct {
	conn net.Conn
	fr   *frameReader
}

func (c *client) next() (inFrame, error) {
	f := <-c.fr.ch
	return f, f.err
}

// expect reads one frame of type want; an ERR comes back as *Error.
func (c *client) expect(want byte) (inFrame, error) {
	f, err := c.next()
	if err != nil {
		return f, err
	}
	if f.t != want {
		return f, fmt.Errorf("%w: got frame 0x%02x, want 0x%02x", ErrProtocol, f.t, want)
	}
	return f, nil
}

func (c *client) write(b []byte) error {
	_, err := c.conn.Write(b)
	return err
}

func (c *client) run(opt Options, res *Result) error {
	// P.
	epoch := time.Now()
	for seq := range opt.Pings {
		if seq > 0 && time.Since(epoch) >= PingWindow {
			break
		}
		sent := time.Since(epoch)
		if err := c.write(frame(FramePing, uint64(seq), uint64(sent))); err != nil {
			return err
		}
		f, err := c.expect(FramePong)
		if err != nil {
			return err
		}
		if u32(f.body[:4]) != uint64(seq) || u64(f.body[4:]) != uint64(sent) {
			return fmt.Errorf("%w: PONG does not echo PING %d", ErrProtocol, seq)
		}
		res.RTTs = append(res.RTTs, f.at.Sub(epoch)-sent)
	}
	rtt := res.RTTMin()

	// D.
	rttUs := min(rtt.Microseconds(), 1<<32-1)
	if err := c.write(frame(FrameDown, uint64(opt.DownBytes), uint64(rttUs))); err != nil {
		return err
	}
	var ack acker
	var t0, t1 time.Time
	for {
		f, err := c.next()
		if err != nil {
			return err
		}
		if f.t == FrameDone {
			res.DownServerSent = int64(u64(f.body[:8]))
			res.DownServerElapsed = time.Duration(u64(f.body[8:])) * time.Microsecond
			break
		}
		if f.t != FrameData {
			return fmt.Errorf("%w: frame 0x%02x during download", ErrProtocol, f.t)
		}
		t1 = f.at
		if t0.IsZero() {
			t0 = t1
		} else {
			res.DownCounted += int64(f.n)
		}
		if ack.data(f.n) {
			if err := c.write(ack.frame()); err != nil {
				return err
			}
		}
	}
	res.DownReceived = ack.received
	res.DownElapsed = t1.Sub(t0)
	if res.DownReceived != res.DownServerSent {
		return fmt.Errorf("%w: received %d download bytes, server sent %d", ErrProtocol, res.DownReceived, res.DownServerSent)
	}
	if err := c.write(ack.frame()); err != nil {
		return err
	}

	// U.
	if err := c.write(frame(FrameUp, uint64(opt.UpBytes))); err != nil {
		return err
	}
	fl := &flow{rtt: rtt}
	handle := func(f inFrame) error {
		if f.err != nil {
			return f.err
		}
		if f.t != FrameAck {
			return fmt.Errorf("%w: frame 0x%02x during upload", ErrProtocol, f.t)
		}
		return fl.ack(int64(u64(f.body)), f.at)
	}
	var seed [32]byte
	crand.Read(seed[:])
	src := rand.NewChaCha8(seed)
	out := make([]byte, 3+MaxData)
	out[0] = FrameData
	start := time.Now()
	window := time.NewTimer(UpWindow)
	defer window.Stop()
stream:
	for fl.sent < opt.UpBytes && time.Since(start) < UpWindow {
		for {
			var f inFrame
			if fl.open() {
				select {
				case f = <-c.fr.ch:
				default:
				}
				if f.at.IsZero() && f.err == nil {
					break
				}
			} else {
				select {
				case f = <-c.fr.ch:
				case <-window.C:
					break stream
				}
			}
			if err := handle(f); err != nil {
				return err
			}
		}
		size := int(min(opt.UpBytes-fl.sent, MaxData))
		out[1], out[2] = byte(size>>8), byte(size)
		src.Read(out[3 : 3+size])
		if err := c.write(out[:3+size]); err != nil {
			return c.refusal(err)
		}
		fl.wrote(size, time.Now())
	}
	res.UpSent = fl.sent
	if err := c.write(frame(FrameEnd, uint64(fl.sent))); err != nil {
		return c.refusal(err)
	}
	// ACKs up to the server's final one, then UPRESULT.
	var f inFrame
	for {
		var err error
		if f, err = c.next(); err != nil {
			return err
		}
		if f.t != FrameAck {
			break
		}
		if err := fl.ack(int64(u64(f.body)), f.at); err != nil {
			return err
		}
	}
	if f.t != FrameUpResult {
		return fmt.Errorf("%w: got frame 0x%02x, want UPRESULT", ErrProtocol, f.t)
	}
	res.UpReceived = int64(u64(f.body[:8]))
	res.UpCounted = int64(u64(f.body[8:16]))
	res.UpElapsed = time.Duration(u64(f.body[16:])) * time.Microsecond
	if fl.acked != fl.sent || res.UpReceived != fl.sent {
		return fmt.Errorf("%w: sent %d upload bytes, %d acknowledged, %d received", ErrProtocol, fl.sent, fl.acked, res.UpReceived)
	}

	// B.
	if err := c.write([]byte{FrameBye}); err != nil {
		return err
	}
	_, err := c.expect(FrameBye)
	return err
}

// refusal explains a failed upload write: the Miner may have sent an ERR
// and closed before the client stopped sending.
func (c *client) refusal(werr error) error {
	timeout := time.After(time.Second)
	for {
		select {
		case f := <-c.fr.ch:
			if e, ok := f.err.(*Error); ok {
				return e
			}
			if f.err != nil {
				return werr
			}
		case <-timeout:
			return werr
		}
	}
}
