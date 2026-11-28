package main

import (
	"bufio"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"time"
)

// The host's remote desktop (Virtualization's own VNC server, which the
// app runs; see HostDesktop there) ends the whole app, machine and all,
// when a viewer sends SetEncodings without the DesktopSize pseudo-encoding:
// it stops on an internal assertion ("It is unclear if we can support
// clients that don't support this pseudo encoding"). A viewer that sends
// no SetEncodings at all is fine. So no viewer talks to it directly: each
// connection passes through here, and every SetEncodings is made to
// include DesktopSize. The display's size does not change, so a viewer
// that does not know DesktopSize is not sent one in practice.
//
// Only the viewer's half of RFB is read; the server's is copied as it is.
// A message this does not know the length of ends the connection: passing
// it on unread would lose track of where the next one starts.

// encodingDesktopSize is a variable, not a constant, so that it can be
// converted to the unsigned number it is sent as.
var encodingDesktopSize int32 = -223

// serveHostDesktop joins each connection accepted on ln with a new one to
// target, the host's remote desktop, through filterViewer, until ctx ends.
func serveHostDesktop(ctx context.Context, ln net.Listener, target string) {
	go func() { <-ctx.Done(); ln.Close() }()
	for {
		c, err := ln.Accept()
		if err != nil {
			if ctx.Err() == nil {
				log.Printf("remote desktop: %v", err)
			}
			return
		}
		go func() {
			defer c.Close()
			h, err := (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, "tcp", target)
			if err != nil {
				log.Printf("remote desktop: %v", err)
				return
			}
			defer h.Close()
			done := make(chan struct{}, 2)
			go func() {
				if err := filterViewer(h, c); err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, net.ErrClosed) {
					log.Printf("remote desktop: viewer cut off: %v", err)
				}
				done <- struct{}{}
			}()
			go func() { io.Copy(c, h); done <- struct{}{} }()
			select {
			case <-done:
			case <-ctx.Done():
			}
		}()
	}
}

// filterViewer copies what a viewer sends from src to the server at dst,
// message by message, adding DesktopSize to each SetEncodings without it.
func filterViewer(dst io.Writer, src io.Reader) error {
	r := bufio.NewReader(src)
	pass := func(n int) error {
		_, err := io.CopyN(dst, r, int64(n))
		return err
	}
	// The handshake: the viewer's version, its choice of security type
	// (not sent by RFB 3.3, where the server decides), the 16-byte answer
	// to VNC authentication, the only type the server offers, and
	// ClientInit.
	version := make([]byte, 12)
	if _, err := io.ReadFull(r, version); err != nil {
		return err
	}
	if _, err := dst.Write(version); err != nil {
		return err
	}
	handshake := 16 + 1
	if string(version) != "RFB 003.003\n" {
		handshake++
	}
	if err := pass(handshake); err != nil {
		return err
	}
	for {
		kind, err := r.ReadByte()
		if err != nil {
			return err
		}
		switch kind {
		case 0: // SetPixelFormat
			err = passWith(dst, r, kind, 19)
		case 2: // SetEncodings
			err = setEncodings(dst, r)
		case 3: // FramebufferUpdateRequest
			err = passWith(dst, r, kind, 9)
		case 4: // KeyEvent
			err = passWith(dst, r, kind, 7)
		case 5: // PointerEvent
			err = passWith(dst, r, kind, 5)
		case 6: // ClientCutText
			var head [7]byte
			if _, err = io.ReadFull(r, head[:]); err != nil {
				return err
			}
			n := binary.BigEndian.Uint32(head[3:])
			if n > 1<<24 {
				return fmt.Errorf("clipboard text of %d bytes", n)
			}
			if _, err = dst.Write(append([]byte{kind}, head[:]...)); err == nil {
				_, err = io.CopyN(dst, r, int64(n))
			}
		case 150: // EnableContinuousUpdates
			err = passWith(dst, r, kind, 9)
		default:
			return fmt.Errorf("unknown message type %d", kind)
		}
		if err != nil {
			return err
		}
	}
}

func passWith(dst io.Writer, r io.Reader, kind byte, n int) error {
	b := make([]byte, 1+n)
	b[0] = kind
	if _, err := io.ReadFull(r, b[1:]); err != nil {
		return err
	}
	_, err := dst.Write(b)
	return err
}

func setEncodings(dst io.Writer, r io.Reader) error {
	var head [3]byte
	if _, err := io.ReadFull(r, head[:]); err != nil {
		return err
	}
	n := int(binary.BigEndian.Uint16(head[1:]))
	list := make([]byte, 4*n)
	if _, err := io.ReadFull(r, list); err != nil {
		return err
	}
	has := false
	for i := 0; i < n; i++ {
		if int32(binary.BigEndian.Uint32(list[4*i:])) == encodingDesktopSize {
			has = true
		}
	}
	if !has {
		list = binary.BigEndian.AppendUint32(list, uint32(encodingDesktopSize))
		n++
	}
	msg := []byte{2, 0, 0, 0}
	binary.BigEndian.PutUint16(msg[2:], uint16(n))
	_, err := dst.Write(append(msg, list...))
	return err
}
