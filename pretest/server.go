package pretest

import (
	"bufio"
	crand "crypto/rand"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"os"
	"time"

	"github.com/nodeexhq/miner/leasetoken"
)

// Report is the server's account of one test.
type Report struct {
	Pings       int
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

// Serve runs the Miner's side of a test on conn, which has just been sent
// the gate's OK at started, within lim (already clamped to the Miner's
// maxima). It always closes conn. The error says why a test that did not
// end in BYE stopped.
func Serve(conn net.Conn, lim leasetoken.TestLimits, started time.Time) (Report, error) {
	defer conn.Close()
	lim = lim.Clamp()
	conn.SetDeadline(started.Add(time.Duration(lim.MaxSeconds) * time.Second))
	s := &server{conn: conn, cnt: &counter{rw: conn}, lim: lim}
	s.r = bufio.NewReaderSize(s.cnt, 32<<10)
	err := s.run()
	s.rep.BytesIn, s.rep.BytesOut = s.cnt.in, s.cnt.out
	if err == nil {
		return s.rep, nil
	}
	var pe *Error
	switch {
	case errors.As(err, &pe):
		// Refused by us (sent below) or by the client (nothing to send).
		if !pe.sent {
			break
		}
		s.sendErr(pe.Code, pe.Reason)
	case errors.Is(err, os.ErrDeadlineExceeded):
		s.sendErr(CodeDeadline, "deadline")
		err = fmt.Errorf("pretest: deadline of %ds reached: %w", lim.MaxSeconds, err)
	case errors.Is(err, ErrProtocol):
		s.sendErr(CodeProtocol, "protocol")
	}
	s.rep.BytesIn, s.rep.BytesOut = s.cnt.in, s.cnt.out
	return s.rep, err
}

type server struct {
	conn net.Conn
	cnt  *counter
	r    *bufio.Reader
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

func (s *server) sendErr(code byte, reason string) {
	if s.midFrame {
		return
	}
	// The test's own deadline may have passed: give the ERR a moment of
	// its own.
	s.conn.SetWriteDeadline(time.Now().Add(250 * time.Millisecond))
	if _, err := s.cnt.Write(errFrame(code, reason)); err != nil {
		return
	}
	// Closing with the client's bytes still unread would reset the
	// connection, which can discard the ERR before the client reads it.
	// Say we are done, and take what the client is still sending for a
	// moment.
	if cw, ok := s.conn.(interface{ CloseWrite() error }); ok && cw.CloseWrite() == nil {
		s.conn.SetReadDeadline(time.Now().Add(lingerDrain))
		io.Copy(io.Discard, s.r)
	}
}

// lingerDrain is how long a refused client's remaining bytes are taken.
const lingerDrain = 500 * time.Millisecond

// next reads a frame and requires it to be one of want.
func (s *server) next(buf []byte, want ...byte) (byte, []byte, error) {
	t, body, err := readFrame(s.r, buf)
	if err != nil {
		return t, nil, err
	}
	for _, w := range want {
		if t == w {
			return t, body, nil
		}
	}
	return t, nil, fmt.Errorf("%w: frame 0x%02x out of order", ErrProtocol, t)
}

func (s *server) run() error {
	buf := make([]byte, MaxData)

	// P: 1 to lim.Pings round trips (none when the limit is 0), then DOWN.
	var down uint64
	for {
		t, body, err := s.next(buf, FramePing, FrameDown)
		if err != nil {
			return err
		}
		if t == FramePing && s.rep.Pings >= s.lim.Pings {
			return refuse(CodeOverLimit, "more than %d pings", s.lim.Pings)
		}
		if t == FrameDown {
			if s.rep.Pings < min(1, s.lim.Pings) {
				return fmt.Errorf("%w: DOWN before any PING", ErrProtocol)
			}
			down = u32(body)
			break
		}
		s.rep.Pings++
		pong := append([]byte{FramePong}, body...)
		if err := s.write(pong); err != nil {
			return err
		}
	}

	// D.
	if down > uint64(s.lim.MaxDownBytes) {
		return refuse(CodeOverLimit, "download of %d bytes, limit %d", down, s.lim.MaxDownBytes)
	}
	if err := s.download(int64(down)); err != nil {
		return err
	}

	// U.
	_, body, err := s.next(buf, FrameUp)
	if err != nil {
		return err
	}
	up := int64(u32(body))
	if up > s.lim.MaxUpBytes {
		return refuse(CodeOverLimit, "upload of %d bytes, limit %d", up, s.lim.MaxUpBytes)
	}
	var t0 time.Time
	var first int64
	for {
		t, body, err := s.next(buf, FrameData, FrameEnd)
		if err != nil {
			return err
		}
		if t == FrameEnd {
			break
		}
		if t0.IsZero() {
			t0, first = time.Now(), int64(len(body))
		}
		s.rep.UpReceived += int64(len(body))
		if s.rep.UpReceived > up {
			return refuse(CodeOverLimit, "upload exceeds the %d bytes requested", up)
		}
	}
	if !t0.IsZero() {
		s.rep.UpElapsed = time.Since(t0)
		s.rep.UpCounted = s.rep.UpReceived - first
	}
	res := frame(FrameUpResult, uint64(s.rep.UpReceived), uint64(s.rep.UpCounted), uint64(s.rep.UpElapsed.Microseconds()))
	if err := s.write(res); err != nil {
		return err
	}

	// B.
	if _, _, err := s.next(buf, FrameBye); err != nil {
		return err
	}
	if err := s.write([]byte{FrameBye}); err != nil {
		return err
	}
	s.rep.Complete = true
	return nil
}

// download streams n random bytes, or as many as DownWindow allows.
func (s *server) download(n int64) error {
	var seed [32]byte
	crand.Read(seed[:])
	src := rand.NewChaCha8(seed)
	out := make([]byte, 3+MaxData)
	out[0] = FrameData
	start := time.Now()
	for s.rep.DownSent < n && time.Since(start) < DownWindow {
		size := int(min(n-s.rep.DownSent, MaxData))
		out[1], out[2] = byte(size>>8), byte(size)
		src.Read(out[3 : 3+size])
		if err := s.write(out[:3+size]); err != nil {
			return err
		}
		s.rep.DownSent += int64(size)
	}
	s.rep.DownElapsed = time.Since(start)
	return s.write(frame(FrameDone, uint64(s.rep.DownSent), uint64(s.rep.DownElapsed.Microseconds())))
}
