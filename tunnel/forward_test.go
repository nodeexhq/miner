package tunnel

import (
	"context"
	"io"
	"net"
	"testing"
	"time"
)

func echoServer(t *testing.T) net.Listener {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				io.Copy(c, c)
			}()
		}
	}()
	t.Cleanup(func() { ln.Close() })
	return ln
}

func startForward(t *testing.T, ctx context.Context, target string) net.Listener {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	dial := func(ctx context.Context) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "tcp", target)
	}
	go Serve(ctx, ln, dial, t.Logf)
	return ln
}

func TestServeForwardsAndHalfCloses(t *testing.T) {
	echo := echoServer(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ln := startForward(t, ctx, echo.Addr().String())

	c, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err := c.Write([]byte("hello")); err != nil {
		t.Fatal(err)
	}
	c.(*net.TCPConn).CloseWrite()

	c.SetReadDeadline(time.Now().Add(5 * time.Second))
	got, err := io.ReadAll(c)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "hello" {
		t.Fatalf("got %q, want %q", got, "hello")
	}
}

func TestServeCancelDropsLiveConnections(t *testing.T) {
	echo := echoServer(t)
	ctx, cancel := context.WithCancel(context.Background())
	ln := startForward(t, ctx, echo.Addr().String())

	c, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.Write([]byte("x"))
	buf := make([]byte, 1)
	c.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.ReadFull(c, buf); err != nil {
		t.Fatal(err)
	}

	cancel()
	c.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := c.Read(buf); err == nil {
		t.Fatal("connection still open after cancel")
	}
	// The listener is closed by its own goroutine, which may run a moment
	// after the one that dropped the connection.
	deadline := time.Now().Add(5 * time.Second)
	for {
		probe, err := net.DialTimeout("tcp", ln.Addr().String(), time.Second)
		if err != nil {
			break
		}
		probe.Close()
		if time.Now().After(deadline) {
			t.Fatal("listener still accepting after cancel")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestParseRule(t *testing.T) {
	r, err := ParseRule("22=192.168.64.5:22")
	if err != nil || r.ListenPort != 22 || r.Target != "192.168.64.5:22" {
		t.Fatalf("got %+v, %v", r, err)
	}
	for _, bad := range []string{"", "22", "22=", "x=1", "70000=1"} {
		if _, err := ParseRule(bad); err == nil {
			t.Errorf("ParseRule(%q): want error", bad)
		}
	}
}
