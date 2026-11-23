package pretest

import (
	crand "crypto/rand"
	"errors"
	"fmt"
	"math/rand/v2"
	"net"
	"os"
	"time"

	"github.com/nvtohq/node/leasetoken"
)

// Report is the server's account of one test.
type Report struct {
	Pings int
	// MinRTT is what the client said in DOWN.
	MinRTT      time.Duration
	DownSent    int64
	DownElapsed time.Duration
	UpReceived  int64
	UpCounted   int64
	UpElapsed   time.Duration
	// Every byte that crossed the connection after the gate's OK, both
	// ways, frame headers included: what the test cost the node's line.
	BytesIn, BytesOut int64
	// Complete is set when the client said BYE.
	Complete bool
}

// Moved is what the test is charged against the node's test budget.
func (r Report) Moved() int64 { return r.BytesIn + r.BytesOut }

// lingerDrain is how long a refused client's remaining bytes are taken.
const lingerDrain = 500 * time.Millisecond

// Serve runs the node's side of a test on conn, which has just been sent
// the gate's OK at started, within lim (clamped to the node's maxima). It
// always closes conn. The error says why a test that did not end in BYE
// stopped.
func Serve(conn net.Conn, lim leasetoken.TestLimits, started time.Time) (Report, error) {
	defer conn.Close()
	lim = lim.Clamp()
	conn.SetDeadline(started.Add(time.Duration(lim.MaxSeconds) * time.Second))
	s := &server{conn: conn, cnt: &counter{rw: conn}, lim: lim}
	s.fr = startReader(s.cnt)
	err := s.run()
	var pe *Error
	switch {
	case err == nil:
		conn.SetReadDeadline(time.Now())
	case errors.As(err, &pe):
		// Refused by us (sent here) or by the client (nothing to send).
		if pe.sent {
			s.sendErr(pe.Code, pe.Reason)
		} else {
			conn.SetReadDeadline(time.Now())
		}
	case errors.Is(err, os.ErrDeadlineExceeded):
		s.sendErr(CodeDeadline, "deadline")
		err = fmt.Errorf("pretest: deadline of %ds reached: %w", lim.MaxSeconds, err)
	case errors.Is(err, ErrProtocol):
		s.sendErr(CodeProtocol, "protocol")
	default:
		conn.SetReadDeadline(time.Now())
	}
	s.fr.stop()
	s.rep.BytesIn, s.rep.BytesOut = s.cnt.in.Load(), s.cnt.out.Load()
	return s.rep, err
}

type server struct {
	conn net.Conn
	cnt  *counter
	fr   *frameReader
	lim  leasetoken.TestLimits
	rep  Report
	// midFrame is set while a frame is partly written; an ERR then would
	// land inside it, so none is sent.
	midFrame bool
}

// refuse is an ERR the server sends.
func refuse(code byte, format string, a ...any) error {
	return &Error{Code: code, Reason: fmt.Sprintf(format, a...), sent: true}
}

func (s *server) write(b []byte) error {
	s.midFrame = true
	_, err := s.cnt.Write(b)
	if err == nil {
		s.midFrame = false
	}
	return err
}

// sendErr sends ERR, then says it is done writing and takes what the client
// still sends for a moment: closing with the client's bytes unread would
// reset the connection, which can discard the ERR before it is read.
func (s *server) sendErr(code byte, reason string) {
	s.conn.SetReadDeadline(time.Now().Add(lingerDrain))
	if s.midFrame {
		return
	}
	// The test's own deadline may have passed: give the ERR a moment.
	s.conn.SetWriteDeadline(time.Now().Add(250 * time.Millisecond))
	if _, err := s.cnt.Write(errFrame(code, reason)); err != nil {
		return
	}
	if cw, ok := s.conn.(interface{ CloseWrite() error }); ok {
		cw.CloseWrite()
	}
}

// next is the client's next frame, or the error that ended reading.
func (s *server) next() (inFrame, error) {
	f := <-s.fr.ch
	return f, f.err
}

func outOfOrder(t byte, phase string) error {
	return fmt.Errorf("%w: frame 0x%02x during %s", ErrProtocol, t, phase)
}

func (s *server) run() error {
	// P: 1 to lim.Pings round trips (none when the limit is 0), then DOWN.
	var f inFrame
	var err error
	for {
		if f, err = s.next(); err != nil {
			return err
		}
		if f.t == FrameDown {
			break
		}
		if f.t != FramePing {
			return outOfOrder(f.t, "ping")
		}
		if s.rep.Pings >= s.lim.Pings {
			return refuse(CodeOverLimit, "more than %d pings", s.lim.Pings)
		}
		s.rep.Pings++
		if err := s.write(append([]byte{FramePong}, f.body...)); err != nil {
			return err
		}
	}
	if s.rep.Pings < min(1, s.lim.Pings) {
		return fmt.Errorf("%w: DOWN before any PING", ErrProtocol)
	}

	// D.
	down := int64(u32(f.body[:4]))
	s.rep.MinRTT = time.Duration(u32(f.body[4:])) * time.Microsecond
	if down > s.lim.MaxDownBytes {
		return refuse(CodeOverLimit, "download of %d bytes, limit %d", down, s.lim.MaxDownBytes)
	}
	fl := &flow{rtt: s.rep.MinRTT}
	if err := s.download(down, fl, f.at); err != nil {
		return err
	}
	// ACKs up to the client's final one, then UP.
	for {
		if f, err = s.next(); err != nil {
			return err
		}
		if f.t != FrameAck {
			break
		}
		if err := fl.ack(int64(u64(f.body)), f.at); err != nil {
			return err
		}
	}
	if f.t != FrameUp {
		return outOfOrder(f.t, "the end of the download")
	}
	if fl.acked != fl.sent {
		return fmt.Errorf("%w: UP with %d of %d download bytes acknowledged", ErrProtocol, fl.acked, fl.sent)
	}

	// U.
	up := int64(u32(f.body))
	if up > s.lim.MaxUpBytes {
		return refuse(CodeOverLimit, "upload of %d bytes, limit %d", up, s.lim.MaxUpBytes)
	}
	var ack acker
	var t0 time.Time
	var first int64
	for {
		if f, err = s.next(); err != nil {
			return err
		}
		if f.t == FrameEnd {
			break
		}
		if f.t != FrameData {
			return outOfOrder(f.t, "upload")
		}
		if t0.IsZero() {
			t0, first = f.at, int64(f.n)
		}
		due := ack.data(f.n)
		s.rep.UpReceived = ack.received
		if ack.received > up {
			return refuse(CodeOverLimit, "upload exceeds the %d bytes requested", up)
		}
		if due {
			if err := s.write(ack.frame()); err != nil {
				return err
			}
		}
	}
	if !t0.IsZero() {
		s.rep.UpElapsed = f.at.Sub(t0)
		s.rep.UpCounted = ack.received - first
	}
	if err := s.write(ack.frame()); err != nil {
		return err
	}
	res := frame(FrameUpResult, uint64(s.rep.UpReceived), uint64(s.rep.UpCounted), uint64(s.rep.UpElapsed.Microseconds()))
	if err := s.write(res); err != nil {
		return err
	}

	// B.
	if f, err = s.next(); err != nil {
		return err
	}
	if f.t != FrameBye {
		return outOfOrder(f.t, "bye")
	}
	if err := s.write([]byte{FrameBye}); err != nil {
		return err
	}
	s.rep.Complete = true
	return nil
}

// download streams n random bytes under flow control until they are sent
// or DownWindow has passed since DOWN arrived at start, then sends DONE.
func (s *server) download(n int64, fl *flow, start time.Time) error {
	var seed [32]byte
	crand.Read(seed[:])
	src := rand.NewChaCha8(seed)
	out := make([]byte, 3+MaxData)
	out[0] = FrameData
	window := time.NewTimer(time.Until(start.Add(DownWindow)))
	defer window.Stop()
	handle := func(f inFrame) error {
		if f.err != nil {
			return f.err
		}
		if f.t != FrameAck {
			return outOfOrder(f.t, "download")
		}
		return fl.ack(int64(u64(f.body)), f.at)
	}
stream:
	for fl.sent < n && time.Since(start) < DownWindow {
		// Take the ACKs that have arrived, and wait for more while the
		// window is full.
		for {
			var f inFrame
			if fl.open() {
				select {
				case f = <-s.fr.ch:
				default:
				}
				if f.at.IsZero() && f.err == nil {
					break
				}
			} else {
				select {
				case f = <-s.fr.ch:
				case <-window.C:
					break stream
				}
			}
			if err := handle(f); err != nil {
				return err
			}
		}
		size := int(min(n-fl.sent, MaxData))
		out[1], out[2] = byte(size>>8), byte(size)
		src.Read(out[3 : 3+size])
		if err := s.write(out[:3+size]); err != nil {
			return err
		}
		fl.wrote(size, time.Now())
	}
	s.rep.DownSent = fl.sent
	s.rep.DownElapsed = time.Since(start)
	return s.write(frame(FrameDone, uint64(fl.sent), uint64(s.rep.DownElapsed.Microseconds())))
}
