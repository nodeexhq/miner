// Package pretest is the connection test a renter's Connect client runs
// against a node before it orders a lease: round trips, a download and an
// upload, measured end to end over the same overlay path a lease would use.
//
// The test is admitted by the gate handshake (service "test", with a test
// token from the platform; see leasetoken.VerifyTest) and is served by the
// Miner process itself. It never reaches the rented machine, and it is
// refused while a lease is active on the node, so it cannot disturb a
// renter.
//
// # Frames
//
// After the gate's "OK\n" both sides exchange frames: a one-byte type and a
// body, all integers unsigned big-endian. Every body is fixed-size except
// DATA's and ERR's.
//
//	type  name      body                                         bytes  sent by
//	0x01  PING      seq u32 | t u64                              12     client
//	0x02  PONG      seq u32 | t u64 (PING's, echoed)             12     server
//	0x10  DOWN      bytes u32 | min_rtt_us u32                   8      client
//	0x11  DATA      len u16 (1..16384) | len bytes               2+len  both
//	0x12  DONE      sent u64 | elapsed_us u64                    16     server
//	0x13  ACK       received u64                                 8      receiver of DATA
//	0x20  UP        bytes u32                                    4      client
//	0x21  END       sent u64                                     8      client
//	0x22  UPRESULT  received u64 | counted u64 | elapsed_us u64  24     server
//	0x30  BYE       (none)                                       0      both
//	0x7F  ERR       code u8 | n u8 | n bytes ASCII reason        2+n    both
//
// ERR codes: 1 protocol, 2 over limit, 3 deadline, 4 budget. Whoever sends
// ERR then closes. Any frame not allowed below where it arrives is ERR 1.
//
// # Phases
//
// The phases run once each, in this order. The Miner closes the connection
// the test's max_seconds (clamped) after it sent OK, sending ERR 3 if it can.
//
// P, ping. The client sends PING and waits for its PONG, one at a time, at
// least once and at most the token's pings times, and stops starting new
// ones PingWindow after the first. t is the client's own clock, echoed
// back. A PING beyond pings is ERR 2. (If the clamped pings is 0, the
// client sends none and DOWN comes first.)
//
// D, download. The client sends DOWN: bytes (at most max_down_bytes, else
// ERR 2) and min_rtt_us, the smallest RTT it measured in P, in
// microseconds. The server streams DATA of random bytes, under the flow
// control below, until it has sent bytes or DownWindow has passed since
// DOWN arrived, then sends DONE: what it sent, and the time from DOWN
// arriving to DONE being written. The client ACKs (below), and on DONE
// sends its final ACK, whose received must equal DONE's sent; then UP.
//
// U, upload. The client sends UP: bytes (at most max_up_bytes, else ERR
// 2), then DATA under flow control until it has sent bytes or UpWindow has
// passed since UP, then END: what it sent. If the server receives more
// DATA than bytes it sends ERR 2. On END the server sends its final ACK,
// then UPRESULT: received; counted, which is received minus the first
// DATA frame's length; and elapsed_us, from the first DATA frame's
// arrival to END's arrival (0 with no DATA).
//
// B, bye. The client sends BYE; the server echoes it and closes.
//
// Throughput is measured by the receiver from DATA timing alone, exactly
// as without ACKs: the bytes after the first frame over the time from the
// first frame's arrival to the last's (download, client side) or to END's
// (upload, server side). Flow control changes what is measured only in
// that a sender ramps up from the window floor: on a long, fast path the
// first round trips carry less than the line could, and a path's rate
// above WindowMax / RTT cannot be reached at all (about 110 Mbit/s at
// 300 ms, 3.3 Gbit/s at 10 ms).
//
// # Flow control
//
// The sender of DATA keeps the bytes it has sent but not seen acknowledged
// within a window, so that when its time is up little is left queued in the
// path and the phase ends about a round trip later rather than when the
// buffers have drained.
//
// The receiver of DATA (client in D, server in U) counts received, the DATA
// payload bytes of the phase, and sends ACK received each time received is
// at least AckEvery more than in its last ACK of the phase, and once more,
// unconditionally, at the phase's end (on DONE, or on END). An ACK for more
// than the sender has sent in the phase, or for less than an earlier ACK of
// the phase, is ERR 1.
//
// The sender writes a DATA frame (of min(remaining, 16384) bytes) only
// while sent - acked < window, where
//
//	window = min(max(WindowMin, 2 × rate × rtt), WindowMax)
//
// rtt is the client's min_rtt_us (from DOWN, for the server; its own, for
// the client); 0 when there is none, which keeps the window at WindowMin.
// rate, in bytes a second, starts at 0 and is updated on every ACK:
//
//   - The sender keeps samples (t, acked). The first is (the time the first
//     DATA frame of the phase was written, 0); every ACK adds (the time it
//     was read, its received).
//   - On an ACK at t with received a, let I = max(rtt, RateMinInterval), and
//     take the newest earlier sample (t', a') with t - t' >= I. If there is
//     one, rate = max(rate, (a - a') / (t - t')). If not, rate stays.
//
// So the rate is the best delivery rate seen over any span of at least one
// round trip, and the window, twice the path's bandwidth-delay product, at
// most doubles each round trip from WindowMin while the window is what
// holds the sender back.
package pretest

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"sync/atomic"
	"time"
)

// Frame types.
const (
	FramePing     byte = 0x01
	FramePong     byte = 0x02
	FrameDown     byte = 0x10
	FrameData     byte = 0x11
	FrameDone     byte = 0x12
	FrameAck      byte = 0x13
	FrameUp       byte = 0x20
	FrameEnd      byte = 0x21
	FrameUpResult byte = 0x22
	FrameBye      byte = 0x30
	FrameErr      byte = 0x7F
)

// ERR codes.
const (
	CodeProtocol  byte = 1
	CodeOverLimit byte = 2
	CodeDeadline  byte = 3
	CodeBudget    byte = 4
)

const (
	// MaxData is the largest DATA payload.
	MaxData = 16384

	// PingWindow is how long the client starts new PINGs, DownWindow how
	// long the server streams a download, UpWindow how long the client
	// streams an upload. They add up to 6.5 s, leaving 3.5 s of a 10 s test
	// for about seven round trips and the window's drain at the end of each
	// stream: enough at an RTT of 300 ms.
	PingWindow = 1500 * time.Millisecond
	DownWindow = 2500 * time.Millisecond
	UpWindow   = 2500 * time.Millisecond

	// Flow control; see the package documentation.
	AckEvery        = 64 << 10
	WindowMin       = 128 << 10
	WindowMax       = 4 << 20
	RateMinInterval = 10 * time.Millisecond
)

// bodyLen is the fixed body length of every frame type but DATA and ERR.
var bodyLen = map[byte]int{
	FramePing: 12, FramePong: 12,
	FrameDown: 8, FrameDone: 16, FrameAck: 8,
	FrameUp: 4, FrameEnd: 8, FrameUpResult: 24,
	FrameBye: 0,
}

// Error is an ERR frame, received from or sent to the other side.
type Error struct {
	Code   byte
	Reason string
	sent   bool // by us, rather than received
}

func (e *Error) Error() string { return fmt.Sprintf("pretest: refused (%d): %s", e.Code, e.Reason) }

// ErrProtocol is a frame that does not belong where it arrived.
var ErrProtocol = errors.New("pretest: protocol violation")

func errFrame(code byte, reason string) []byte {
	if len(reason) > 255 {
		reason = reason[:255]
	}
	return append([]byte{FrameErr, code, byte(len(reason))}, reason...)
}

func frame(t byte, fields ...uint64) []byte {
	b := []byte{t}
	switch t {
	case FramePing, FramePong:
		b = binary.BigEndian.AppendUint32(b, uint32(fields[0]))
		b = binary.BigEndian.AppendUint64(b, fields[1])
	case FrameDown:
		b = binary.BigEndian.AppendUint32(b, uint32(fields[0]))
		b = binary.BigEndian.AppendUint32(b, uint32(fields[1]))
	case FrameUp:
		b = binary.BigEndian.AppendUint32(b, uint32(fields[0]))
	default:
		for _, f := range fields {
			b = binary.BigEndian.AppendUint64(b, f)
		}
	}
	return b
}

// readFrame reads one frame. For DATA, body holds the payload (in buf when
// it fits). For ERR it returns the frame as an *Error.
func readFrame(r io.Reader, buf []byte) (t byte, body []byte, err error) {
	var hdr [3]byte
	if _, err := io.ReadFull(r, hdr[:1]); err != nil {
		return 0, nil, err
	}
	t = hdr[0]
	switch t {
	case FrameData:
		if _, err := io.ReadFull(r, hdr[1:3]); err != nil {
			return t, nil, eof(err)
		}
		n := int(binary.BigEndian.Uint16(hdr[1:3]))
		if n < 1 || n > MaxData {
			return t, nil, fmt.Errorf("%w: DATA of %d bytes", ErrProtocol, n)
		}
		if len(buf) < n {
			buf = make([]byte, n)
		}
		body = buf[:n]
		_, err = io.ReadFull(r, body)
		return t, body, eof(err)
	case FrameErr:
		if _, err := io.ReadFull(r, hdr[1:3]); err != nil {
			return t, nil, eof(err)
		}
		reason := make([]byte, hdr[2])
		if _, err := io.ReadFull(r, reason); err != nil {
			return t, nil, eof(err)
		}
		return t, nil, &Error{Code: hdr[1], Reason: string(reason)}
	}
	n, ok := bodyLen[t]
	if !ok {
		return t, nil, fmt.Errorf("%w: unknown frame type 0x%02x", ErrProtocol, t)
	}
	if len(buf) < n {
		buf = make([]byte, n)
	}
	body = buf[:n]
	_, err = io.ReadFull(r, body)
	return t, body, eof(err)
}

// eof turns an EOF inside a frame into ErrUnexpectedEOF.
func eof(err error) error {
	if err == io.EOF {
		return io.ErrUnexpectedEOF
	}
	return err
}

func u32(b []byte) uint64 { return uint64(binary.BigEndian.Uint32(b)) }
func u64(b []byte) uint64 { return binary.BigEndian.Uint64(b) }

// counter counts the bytes moved in each direction.
type counter struct {
	rw      io.ReadWriter
	in, out atomic.Int64
}

func (c *counter) Read(b []byte) (int, error) {
	n, err := c.rw.Read(b)
	c.in.Add(int64(n))
	return n, err
}

func (c *counter) Write(b []byte) (int, error) {
	n, err := c.rw.Write(b)
	c.out.Add(int64(n))
	return n, err
}

// inFrame is a frame as the reader delivers it: its type, its body (nil
// for DATA, whose payload is only counted), when it was read, or the error
// that ended reading.
type inFrame struct {
	t    byte
	body []byte
	n    int // DATA payload length
	at   time.Time
	err  error
}

// frameReader reads frames in a goroutine of its own, so that a side can
// take ACKs while it streams DATA and send ACKs while it reads them. Its
// channel ends with a frame carrying an error.
type frameReader struct {
	ch     chan inFrame
	quit   chan struct{}
	exited chan struct{}
}

func startReader(r io.Reader) *frameReader {
	fr := &frameReader{ch: make(chan inFrame, 64), quit: make(chan struct{}), exited: make(chan struct{})}
	go func() {
		defer close(fr.exited)
		br := bufio.NewReaderSize(r, 64<<10)
		buf := make([]byte, MaxData)
		for {
			t, body, err := readFrame(br, buf)
			f := inFrame{t: t, at: time.Now(), err: err}
			if err == nil {
				if t == FrameData {
					f.n = len(body)
				} else {
					f.body = append([]byte(nil), body...)
				}
			}
			// After stop, frames are read and dropped until the
			// connection ends: a drain.
			select {
			case fr.ch <- f:
			case <-fr.quit:
			}
			if err != nil {
				return
			}
		}
	}()
	return fr
}

// stop ends delivery and waits for the reader to finish; the caller first
// makes its reads end (a read deadline, or closing the connection).
func (fr *frameReader) stop() {
	select {
	case <-fr.quit:
	default:
		close(fr.quit)
	}
	<-fr.exited
}

// flow is the sending side's flow control; see the package documentation.
type flow struct {
	rtt         time.Duration
	sent, acked int64
	rate        float64 // bytes per second
	samples     []ackSample
}

type ackSample struct {
	at    time.Time
	acked int64
}

func (f *flow) window() int64 {
	w := int64(2 * f.rate * f.rtt.Seconds())
	return min(max(w, WindowMin), WindowMax)
}

// open says whether another DATA frame may be written.
func (f *flow) open() bool { return f.sent-f.acked < f.window() }

// wrote records a DATA frame written at t.
func (f *flow) wrote(n int, t time.Time) {
	if len(f.samples) == 0 {
		f.samples = append(f.samples, ackSample{t, 0})
	}
	f.sent += int64(n)
}

// ack records an ACK for received read at t.
func (f *flow) ack(received int64, t time.Time) error {
	if received > f.sent || received < f.acked {
		return fmt.Errorf("%w: ACK for %d bytes, %d sent, %d acknowledged before", ErrProtocol, received, f.sent, f.acked)
	}
	f.acked = received
	iv := max(f.rtt, RateMinInterval)
	for j := len(f.samples) - 1; j >= 0; j-- {
		if d := t.Sub(f.samples[j].at); d >= iv {
			f.rate = max(f.rate, float64(received-f.samples[j].acked)/d.Seconds())
			break
		}
	}
	f.samples = append(f.samples, ackSample{t, received})
	return nil
}

// acker is the receiving side: when to ACK.
type acker struct{ received, acked int64 }

// data counts a DATA frame and says whether to ACK now.
func (a *acker) data(n int) bool {
	a.received += int64(n)
	return a.received-a.acked >= AckEvery
}

func (a *acker) frame() []byte {
	a.acked = a.received
	return frame(FrameAck, uint64(a.received))
}
