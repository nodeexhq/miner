package pretest

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"testing"
	"time"

	"github.com/nodeexhq/miner/leasetoken"
)

// pair returns the two ends of a loopback TCP connection, so the test runs
// over a real socket with real buffering.
func pair(t *testing.T) (client, server net.Conn) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	accepted := make(chan net.Conn, 1)
	go func() {
		c, _ := ln.Accept()
		accepted <- c
	}()
	client, err = net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	server = <-accepted
	t.Cleanup(func() { client.Close(); server.Close() })
	return client, server
}

type served struct {
	rep Report
	err error
}

func serve(server net.Conn, lim leasetoken.TestLimits) <-chan served {
	done := make(chan served, 1)
	go func() {
		rep, err := Serve(server, lim, time.Now())
		done <- served{rep, err}
	}()
	return done
}

func TestFullTest(t *testing.T) {
	client, server := pair(t)
	lim := leasetoken.DefaultTestLimits()
	done := serve(server, lim)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	res, err := Run(ctx, client, OptionsFor(lim))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	s := <-done
	if s.err != nil || !s.rep.Complete {
		t.Fatalf("Serve: %+v, %v", s.rep, s.err)
	}

	if len(res.RTTs) != lim.Pings || s.rep.Pings != lim.Pings {
		t.Errorf("%d RTTs, server saw %d pings; want %d", len(res.RTTs), s.rep.Pings, lim.Pings)
	}
	if m := res.RTTMin(); m <= 0 || res.RTTMedian() > 50*time.Millisecond {
		t.Errorf("loopback RTT min %s median %s", m, res.RTTMedian())
	}
	if res.DownReceived != lim.MaxDownBytes || res.DownServerSent != lim.MaxDownBytes || s.rep.DownSent != lim.MaxDownBytes {
		t.Errorf("download: received %d, server sent %d (%d)", res.DownReceived, res.DownServerSent, s.rep.DownSent)
	}
	if res.DownCounted != res.DownReceived-MaxData || res.DownElapsed <= 0 || res.DownServerElapsed <= 0 {
		t.Errorf("download counted %d of %d in %s (server %s)", res.DownCounted, res.DownReceived, res.DownElapsed, res.DownServerElapsed)
	}
	if res.UpSent != lim.MaxUpBytes || res.UpReceived != res.UpSent || s.rep.UpReceived != res.UpSent {
		t.Errorf("upload: sent %d, received %d (%d)", res.UpSent, res.UpReceived, s.rep.UpReceived)
	}
	if res.UpCounted != res.UpReceived-MaxData || res.UpElapsed <= 0 {
		t.Errorf("upload counted %d of %d in %s", res.UpCounted, res.UpReceived, res.UpElapsed)
	}
	// Loopback moves 10 MB in well under a second; anything slower than
	// 50 Mbit/s here means the measurement is wrong, not the link.
	if res.DownMbps() < 50 || res.UpMbps() < 50 {
		t.Errorf("throughput down %.0f, up %.0f Mbit/s", res.DownMbps(), res.UpMbps())
	}
	// What the test is charged: all of it, both ways, headers included.
	payload := lim.MaxDownBytes + lim.MaxUpBytes
	if m := s.rep.Moved(); m < payload || m > payload+payload/100 {
		t.Errorf("moved %d bytes for %d of payload", m, payload)
	}
	t.Logf("rtt %s, down %.0f Mbit/s, up %.0f Mbit/s, moved %d", res.RTTMedian(), res.DownMbps(), res.UpMbps(), s.rep.Moved())
}

// The client may ask for less than the token allows, down to nothing.
func TestEmptyTest(t *testing.T) {
	client, server := pair(t)
	done := serve(server, leasetoken.TestLimits{MaxSeconds: 5})
	res, err := Run(context.Background(), client, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if s := <-done; s.err != nil || !s.rep.Complete || res.DownReceived != 0 || res.UpReceived != 0 || res.UpElapsed != 0 {
		t.Errorf("empty test: %+v / %+v, %v", res, s.rep, s.err)
	}
}

func wantRefusal(t *testing.T, name string, err error, code byte) {
	t.Helper()
	var e *Error
	if !errors.As(err, &e) || e.Code != code {
		t.Errorf("%s: %v, want ERR code %d", name, err, code)
	}
}

func TestClientOverLimits(t *testing.T) {
	lim := leasetoken.TestLimits{Pings: 3, MaxDownBytes: 50_000, MaxUpBytes: 50_000, MaxSeconds: 5}
	for name, opt := range map[string]Options{
		"too many pings":   {Pings: 4},
		"download too big": {Pings: 3, DownBytes: 50_001},
		"upload too big":   {Pings: 3, DownBytes: 1000, UpBytes: 2_000_000},
	} {
		client, server := pair(t)
		done := serve(server, lim)
		_, err := Run(context.Background(), client, opt)
		wantRefusal(t, name, err, CodeOverLimit)
		if s := <-done; s.err == nil || s.rep.Complete {
			t.Errorf("%s: server %+v, %v", name, s.rep, s.err)
		}
	}
}

// raw drives the server frame by frame, as a misbehaving client would.
type raw struct {
	t    *testing.T
	conn net.Conn
}

func (r raw) send(b ...[]byte) {
	for _, f := range b {
		if _, err := r.conn.Write(f); err != nil {
			r.t.Logf("write: %v", err)
			return
		}
	}
}

// until reads frames until an ERR or the end, and returns the ERR.
func (r raw) until() error {
	for {
		_, _, err := readFrame(r.conn, nil)
		if err != nil {
			r.conn.(*net.TCPConn).CloseWrite()
			return err
		}
	}
}

func data(n int) []byte {
	b := []byte{FrameData, 0, 0}
	binary.BigEndian.PutUint16(b[1:], uint16(n))
	return append(b, make([]byte, n)...)
}

func TestUploadBeyondRequest(t *testing.T) {
	client, server := pair(t)
	done := serve(server, leasetoken.TestLimits{MaxDownBytes: 0, MaxUpBytes: 1000, MaxSeconds: 5})
	r := raw{t, client}
	go r.send(frame(FrameDown, 0, 0), frame(FrameUp, 100), data(60), data(60))
	wantRefusal(t, "upload beyond its request", r.until(), CodeOverLimit)
	if s := <-done; s.rep.UpReceived != 120 || s.rep.Complete {
		t.Errorf("server: %+v, %v", s.rep, s.err)
	}
}

func TestMalformedFrames(t *testing.T) {
	lim := leasetoken.TestLimits{Pings: 1, MaxDownBytes: 1000, MaxUpBytes: 1000, MaxSeconds: 5}
	ping := frame(FramePing, 0, 0)
	for name, frames := range map[string][][]byte{
		"unknown type":        {{0x55}},
		"empty DATA":          {ping, frame(FrameDown, 0, 0), frame(FrameUp, 10), {FrameData, 0, 0}},
		"oversized DATA":      {ping, frame(FrameDown, 0, 0), frame(FrameUp, 10), {FrameData, 0x50, 0}},
		"DOWN before pings":   {frame(FrameDown, 10, 0)},
		"UP before DOWN":      {ping, frame(FrameUp, 10)},
		"BYE first":           {{FrameBye}},
		"DATA during pings":   {data(10)},
		"server frame":        {ping, {FramePong, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0}},
		"second DOWN":         {ping, frame(FrameDown, 0, 0), frame(FrameDown, 0, 0)},
		"BYE before UPRESULT": {ping, frame(FrameDown, 0, 0), frame(FrameUp, 10), {FrameBye}},
		"ACK during pings":    {ping, frame(FrameAck, 0)},
		"ACK beyond sent":     {ping, frame(FrameDown, 0, 0), frame(FrameAck, 5)},
		"ACK during upload":   {ping, frame(FrameDown, 0, 0), frame(FrameUp, 10), frame(FrameAck, 0)},
		"UP before final ACK": {ping, frame(FrameDown, 1000, 0), frame(FrameUp, 10)},
	} {
		client, server := pair(t)
		done := serve(server, lim)
		r := raw{t, client}
		go r.send(frames...)
		wantRefusal(t, name, r.until(), CodeProtocol)
		if s := <-done; s.err == nil || s.rep.Complete {
			t.Errorf("%s: server %+v, %v", name, s.rep, s.err)
		}
	}

	// A frame cut short ends the test without an answer.
	client, server := pair(t)
	done := serve(server, lim)
	client.Write([]byte{FramePing, 0, 0})
	client.(*net.TCPConn).CloseWrite()
	if s := <-done; !errors.Is(s.err, io.ErrUnexpectedEOF) {
		t.Errorf("truncated frame: %v", s.err)
	}
}

// The Miner closes the test MaxSeconds after OK, whatever the client does.
func TestDeadline(t *testing.T) {
	client, server := pair(t)
	start := time.Now()
	done := serve(server, leasetoken.TestLimits{Pings: 5, MaxSeconds: 1})
	r := raw{t, client}
	r.send(frame(FramePing, 0, 0)) // then nothing
	wantRefusal(t, "stalled client", r.until(), CodeDeadline)
	s := <-done
	if el := time.Since(start); el < time.Second || el > 2*time.Second {
		t.Errorf("closed after %s, want 1s", el)
	}
	if !containsDeadline(s.err) || s.rep.Pings != 1 {
		t.Errorf("server: %+v, %v", s.rep, s.err)
	}
	// The connection is closed, not just answered.
	client.SetReadDeadline(time.Now().Add(time.Second))
	if _, err := client.Read(make([]byte, 1)); err != io.EOF {
		t.Errorf("after the deadline: %v", err)
	}

	// A client that keeps the server busy past the deadline (here by not
	// reading the download) is cut off just the same.
	client, server = pair(t)
	start = time.Now()
	done = serve(server, leasetoken.TestLimits{MaxDownBytes: 10_000_000, MaxSeconds: 1})
	client.Write(frame(FrameDown, 10_000_000, 0))
	if s := <-done; s.err == nil || time.Since(start) > 2*time.Second {
		t.Errorf("unread download: %v after %s", s.err, time.Since(start))
	}

	// And a client running the test gets the refusal, not a hang.
	client, server = pair(t)
	done = serve(server, leasetoken.TestLimits{Pings: 50, MaxSeconds: 0})
	_, err := Run(context.Background(), client, Options{Pings: 50})
	wantRefusal(t, "zero seconds", err, CodeDeadline)
	<-done
}

func containsDeadline(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}
