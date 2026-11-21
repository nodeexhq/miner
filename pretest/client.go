package pretest

import (
	"bufio"
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
// conn.
func Run(ctx context.Context, conn net.Conn, opt Options) (Result, error) {
	var res Result
	if opt.Pings < 0 || opt.DownBytes < 0 || opt.UpBytes < 0 ||
		opt.DownBytes > 1<<32-1 || opt.UpBytes > 1<<32-1 {
		return res, fmt.Errorf("pretest: invalid options %+v", opt)
	}
	if d, ok := ctx.Deadline(); ok {
		conn.SetDeadline(d)
		defer conn.SetDeadline(time.Time{})
	}
	stop := context.AfterFunc(ctx, func() { conn.SetDeadline(time.Unix(1, 0)) })
	defer stop()
	c := &client{conn: conn, r: bufio.NewReaderSize(conn, 64<<10), buf: make([]byte, MaxData)}
	err := c.run(opt, &res)
	if err != nil && ctx.Err() != nil {
		err = fmt.Errorf("pretest: %w (%w)", ctx.Err(), err)
	}
	return res, err
}

type client struct {
	conn net.Conn
	r    *bufio.Reader
	buf  []byte
}

// expect reads one frame of type want; an ERR comes back as *Error.
func (c *client) expect(want byte) ([]byte, error) {
	t, body, err := readFrame(c.r, c.buf)
	if err != nil {
		return nil, err
	}
	if t != want {
		return nil, fmt.Errorf("%w: got frame 0x%02x, want 0x%02x", ErrProtocol, t, want)
	}
	return body, nil
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
		body, err := c.expect(FramePong)
		if err != nil {
			return err
		}
		if u32(body[:4]) != uint64(seq) || u64(body[4:]) != uint64(sent) {
			return fmt.Errorf("%w: PONG does not echo PING %d", ErrProtocol, seq)
		}
		res.RTTs = append(res.RTTs, time.Since(epoch)-sent)
	}

	// D.
	if err := c.write(frame(FrameDown, uint64(opt.DownBytes))); err != nil {
		return err
	}
	var t0, t1 time.Time
	for {
		t, body, err := readFrame(c.r, c.buf)
		if err != nil {
			return err
		}
		if t == FrameDone {
			res.DownServerSent = int64(u64(body[:8]))
			res.DownServerElapsed = time.Duration(u64(body[8:])) * time.Microsecond
			break
		}
		if t != FrameData {
			return fmt.Errorf("%w: frame 0x%02x during download", ErrProtocol, t)
		}
		t1 = time.Now()
		if t0.IsZero() {
			t0 = t1
		} else {
			res.DownCounted += int64(len(body))
		}
		res.DownReceived += int64(len(body))
	}
	res.DownElapsed = t1.Sub(t0)
	if res.DownReceived != res.DownServerSent {
		return fmt.Errorf("%w: received %d download bytes, server sent %d", ErrProtocol, res.DownReceived, res.DownServerSent)
	}

	// U.
	if err := c.write(frame(FrameUp, uint64(opt.UpBytes))); err != nil {
		return err
	}
	var seed [32]byte
	crand.Read(seed[:])
	src := rand.NewChaCha8(seed)
	out := make([]byte, 3+MaxData)
	out[0] = FrameData
	start := time.Now()
	for res.UpSent < opt.UpBytes && time.Since(start) < UpWindow {
		size := int(min(opt.UpBytes-res.UpSent, MaxData))
		out[1], out[2] = byte(size>>8), byte(size)
		src.Read(out[3 : 3+size])
		if err := c.write(out[:3+size]); err != nil {
			return c.refusal(err)
		}
		res.UpSent += int64(size)
	}
	if err := c.write(frame(FrameEnd, uint64(res.UpSent))); err != nil {
		return c.refusal(err)
	}
	body, err := c.expect(FrameUpResult)
	if err != nil {
		return err
	}
	res.UpReceived = int64(u64(body[:8]))
	res.UpCounted = int64(u64(body[8:16]))
	res.UpElapsed = time.Duration(u64(body[16:])) * time.Microsecond

	// B.
	if err := c.write([]byte{FrameBye}); err != nil {
		return err
	}
	_, err = c.expect(FrameBye)
	return err
}

// refusal explains a failed upload write: the Miner may have sent an ERR
// and closed before the client stopped sending.
func (c *client) refusal(werr error) error {
	c.conn.SetReadDeadline(time.Now().Add(time.Second))
	if _, _, err := readFrame(c.r, c.buf); err != nil {
		if e, ok := err.(*Error); ok {
			return e
		}
	}
	return werr
}
