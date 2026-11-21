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
// After the gate's "OK\n" both sides exchange frames: a one-byte type and a
// fixed body, all integers unsigned big-endian. The phases run once each, in
// order:
//
//	P  C: PING seq:u32 t:u64        S: PONG seq:u32 t:u64     (1 to Pings times)
//	D  C: DOWN bytes:u32            S: DATA... then DONE sent:u64 elapsed_us:u64
//	U  C: UP bytes:u32, DATA..., END sent:u64
//	                                S: UPRESULT received:u64 counted:u64 elapsed_us:u64
//	B  C: BYE                       S: BYE, and closes
//
// DATA is len:u16 (1..16384) followed by that many bytes. The server stops a
// download when the requested bytes are sent or DownWindow has passed since
// DOWN; the client stops pinging after PingWindow and uploading after
// UpWindow, so that the whole test fits in its 10 seconds on a slow line
// with a long round trip. Either side may send ERR code:u8 n:u8 reason:n bytes and close. The
// Miner closes the connection the test's MaxSeconds after it sent OK.
package pretest

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"time"
)

// Frame types.
const (
	FramePing     byte = 0x01
	FramePong     byte = 0x02
	FrameDown     byte = 0x10
	FrameData     byte = 0x11
	FrameDone     byte = 0x12
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
	// PingWindow is how long the client goes on pinging (it sends at
	// least one PING, and at most the token's pings). DownWindow is how
	// long the server streams a download at most, and UpWindow how long
	// the client streams an upload at most. They add up to 8.5 s, leaving
	// 1.5 s of a 10 s test for some six round trips and for whatever the
	// path's buffers still hold when a window closes: enough at 150 ms on
	// a fast line, not at 300 ms or on a slow line with deep buffers.
	PingWindow = 2500 * time.Millisecond
	DownWindow = 3000 * time.Millisecond
	UpWindow   = 3000 * time.Millisecond
)

// bodyLen is the fixed body length of every frame type but DATA and ERR.
var bodyLen = map[byte]int{
	FramePing: 12, FramePong: 12,
	FrameDown: 4, FrameDone: 16,
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
	case FrameDown, FrameUp:
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
	in, out int64
}

func (c *counter) Read(b []byte) (int, error) {
	n, err := c.rw.Read(b)
	c.in += int64(n)
	return n, err
}

func (c *counter) Write(b []byte) (int, error) {
	n, err := c.rw.Write(b)
	c.out += int64(n)
	return n, err
}
