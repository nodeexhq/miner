// Package tunnel holds the TCP forwarding shared by the Miner and Connect
// prototypes.
package tunnel

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
)

// DialFunc opens the upstream side of a forwarded connection.
type DialFunc func(ctx context.Context) (net.Conn, error)

// Logf matches log.Printf.
type Logf func(format string, args ...any)

// Serve accepts connections on ln and pipes each one to a connection obtained
// from dial. It returns when ln is closed or ctx is cancelled; live connections
// are closed on cancellation.
func Serve(ctx context.Context, ln net.Listener, dial DialFunc, logf Logf) error {
	var wg sync.WaitGroup
	defer wg.Wait()

	stop := context.AfterFunc(ctx, func() { ln.Close() })
	defer stop()

	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return nil
			}
			return err
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			handle(ctx, conn, dial, logf)
		}()
	}
}

func handle(ctx context.Context, down net.Conn, dial DialFunc, logf Logf) {
	defer down.Close()

	up, err := dial(ctx)
	if err != nil {
		logf("forward %s: dial upstream: %v", down.RemoteAddr(), err)
		return
	}
	defer up.Close()

	logf("forward %s -> %s: open", down.RemoteAddr(), up.RemoteAddr())
	Pipe(ctx, down, up)
	logf("forward %s -> %s: closed", down.RemoteAddr(), up.RemoteAddr())
}

// Pipe copies between a and b until both directions finish or ctx is
// cancelled, in which case both connections are closed.
func Pipe(ctx context.Context, a, b net.Conn) {
	stop := context.AfterFunc(ctx, func() {
		a.Close()
		b.Close()
	})
	defer stop()

	var wg sync.WaitGroup
	wg.Add(2)
	go pipe(&wg, a, b)
	go pipe(&wg, b, a)
	wg.Wait()
}

// pipe copies src to dst, then half-closes dst so the peer sees EOF while the
// opposite direction keeps flowing.
func pipe(wg *sync.WaitGroup, dst, src net.Conn) {
	defer wg.Done()
	io.Copy(dst, src)
	if cw, ok := dst.(interface{ CloseWrite() error }); ok {
		cw.CloseWrite()
	} else {
		dst.Close()
	}
}

// Rule is one "listen=target" forwarding rule.
type Rule struct {
	ListenPort int
	Target     string
}

// ParseRule parses "PORT=TARGET", for example "22=192.168.64.5:22" on the
// Miner or "10022=22" on Connect.
func ParseRule(s string) (Rule, error) {
	port, target, ok := strings.Cut(s, "=")
	if !ok || target == "" {
		return Rule{}, fmt.Errorf("rule %q: want PORT=TARGET", s)
	}
	p, err := strconv.Atoi(port)
	if err != nil || p < 1 || p > 65535 {
		return Rule{}, fmt.Errorf("rule %q: bad listen port", s)
	}
	return Rule{ListenPort: p, Target: target}, nil
}

// Rules is a repeatable flag of forwarding rules.
type Rules []Rule

func (r *Rules) String() string {
	parts := make([]string, len(*r))
	for i, x := range *r {
		parts[i] = fmt.Sprintf("%d=%s", x.ListenPort, x.Target)
	}
	return strings.Join(parts, ",")
}

func (r *Rules) Set(s string) error {
	rule, err := ParseRule(s)
	if err != nil {
		return err
	}
	*r = append(*r, rule)
	return nil
}
