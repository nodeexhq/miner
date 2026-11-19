package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/netip"
	"slices"
	"strings"
	"sync/atomic"
	"time"
)

// The owner's public address is on the internet like any other, so the
// private-network rules do not stop a rented machine from sending to it.
// Many home routers then loop such traffic back inside (NAT loopback):
// to services the owner forwarded from the internet, like a NAS or a
// camera, and on some routers to their own admin page. The helper learns
// that address from the platform and refuses it too.

// keepHome is how many recent addresses stay refused: a home address can
// change, and connections opened to the last one should not keep working.
const keepHome = 4

// homeAddrs is the set of the owner's recent public addresses, newest first.
type homeAddrs struct{ p atomic.Pointer[[]netip.Addr] }

func (h *homeAddrs) list() []netip.Addr {
	if l := h.p.Load(); l != nil {
		return *l
	}
	return nil
}

// add puts a at the front and reports whether it is new.
func (h *homeAddrs) add(a netip.Addr) bool {
	old := h.list()
	if len(old) > 0 && old[0] == a {
		return false
	}
	next := append([]netip.Addr{a}, slices.DeleteFunc(slices.Clone(old), func(x netip.Addr) bool { return x == a })...)
	if len(next) > keepHome {
		next = next[:keepHome]
	}
	h.p.Store(&next)
	return true
}

// ipv4Client reaches the platform over IPv4 only: the machine's network is
// IPv4, so the IPv4 address is the one that matters.
var ipv4Client = &http.Client{
	Timeout: 15 * time.Second,
	Transport: &http.Transport{
		Proxy: nil,
		DialContext: func(ctx context.Context, _, addr string) (net.Conn, error) {
			return (&net.Dialer{Timeout: 10 * time.Second}).DialContext(ctx, "tcp4", addr)
		},
		TLSHandshakeTimeout: 10 * time.Second,
	},
}

// lookupHome asks the platform which address this Mac's traffic comes from.
func lookupHome(ctx context.Context, api string) (netip.Addr, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", strings.TrimRight(api, "/")+"/v1/ip", nil)
	if err != nil {
		return netip.Addr{}, err
	}
	res, err := ipv4Client.Do(req)
	if err != nil {
		return netip.Addr{}, err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return netip.Addr{}, fmt.Errorf("status %d", res.StatusCode)
	}
	var body struct{ IP string }
	if err := json.NewDecoder(io.LimitReader(res.Body, 4096)).Decode(&body); err != nil {
		return netip.Addr{}, err
	}
	a, err := netip.ParseAddr(body.IP)
	if err != nil {
		return netip.Addr{}, err
	}
	a = a.Unmap()
	if !a.Is4() {
		return netip.Addr{}, fmt.Errorf("not an IPv4 address: %s", a)
	}
	return a, nil
}

// watchHome keeps h current: soon after start until the first answer,
// then every few minutes, since a home address can change at any time.
func watchHome(ctx context.Context, api string, h *homeAddrs) {
	wait := time.Duration(0)
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		a, err := lookupHome(ctx, api)
		switch {
		case err != nil:
			if ctx.Err() == nil {
				log.Printf("could not learn the owner's public address: %v", err)
			}
			if len(h.list()) == 0 {
				wait = 15 * time.Second
			} else {
				wait = 5 * time.Minute
			}
			continue
		case h.add(a):
			log.Printf("refusing the owner's public address %s", a)
		}
		wait = 5 * time.Minute
	}
}
