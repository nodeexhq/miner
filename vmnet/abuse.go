package main

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"sync"
	"sync/atomic"
	"time"
)

// Abuse of the owner's address.
//
// The bandwidth limits keep a rented machine from taking the owner's line,
// but the things that get a home address blocked or reported use very
// little of it: scanning for open ports or hosts, guessing passwords,
// floods of connection attempts (SYN floods) and of small UDP packets.
// What the machine sends is watched for those patterns here, per
// destination, in tables that never grow past a fixed size.
//
// There are two kinds of response.
//
// Soft: past a rate (new connections or flows per second, UDP packets per
// second), the excess frames are dropped and counted. A dropped SYN is sent
// again by the machine a second later; a dropped UDP packet is what UDP
// expects anyway. Normal work stays well under the rates, and a short burst
// over them costs a second's delay, nothing more.
//
// Hard: a pattern that normal work does not make (many hosts or many ports
// of one host contacted in a minute, many connection attempts left
// unanswered, a rate exceeded without pause for half a minute) cuts the
// machine's network both ways and reports it, once, as a JSON line on
// standard output. The cut is sticky: see filteredConn.leaseHold.
//
// Only what the machine starts counts. Its own small network (the gateway's
// DNS, and the SSH and remote desktop that reach it through the gateway) is
// not watched, and frames to the machine are looked at only to see whether
// a connection attempt was answered.

// abuseLimits are the thresholds, read from the "abuse" object of the
// limits file the app writes (bandwidth.json). Every field is optional:
// zero (or absent) is the default below, a negative value turns that one
// check off, and "off": true turns them all off.
type abuseLimits struct {
	Off bool `json:"off,omitempty"`
	// New outbound flows: TCP connection attempts (a SYN without ACK) and
	// UDP or ping flows (the first packet to a host and port in a minute).
	NewFlowsPerSec float64 `json:"new_flows_per_sec,omitempty"`
	NewFlowsBurst  float64 `json:"new_flows_burst,omitempty"`
	// UDP (and ping) packets per second to destinations that have not
	// answered in the last few seconds.
	UDPPerSec float64 `json:"udp_pps,omitempty"`
	UDPBurst  float64 `json:"udp_burst,omitempty"`
	// All UDP (and ping) packets per second, answered or not.
	UDPAllPerSec float64 `json:"udp_all_pps,omitempty"`
	UDPAllBurst  float64 `json:"udp_all_burst,omitempty"`
	// Patterns, each counted over the last minute.
	DistinctIPs    int `json:"distinct_ips_per_min,omitempty"`
	PortsPerIP     int `json:"ports_per_ip_per_min,omitempty"`
	UnansweredSYNs int `json:"unanswered_syns_per_min,omitempty"`
	// How long a soft limit may be exceeded without pause before it counts
	// as a pattern.
	SustainedSec float64 `json:"sustained_sec,omitempty"`
}

// The defaults, and why.
//
// New flows, 60 per second with a burst of 200. A heavy web page opens
// 50 to 150 connections in its first second or two (browsers keep at most
// six per host over HTTP/1.1 and share one over HTTP/2 and QUIC); brew,
// npm, pip, cargo, go and git reuse a handful of kept-alive connections;
// Xcode and macOS downloads use a few. The burst covers a page, or a few at
// once; 60 a second after that is more than a person or a build makes, and
// far below what a SYN flood or a scanner wants (thousands a second).
// Going over only delays the extra connections by a second.
//
// UDP to quiet destinations, 2000 packets per second with a burst of 4000.
// "Quiet" is a host and port that sent nothing back in the last 10 s: a
// flood target, or a scan. Calls, QUIC, DNS and the remote desktop relays
// (Jump Desktop, UU Remote) all hear back many times a second and are not
// counted here. 2000 pps is still a 1080p call ten times over.
//
// All UDP, 10000 packets per second with a burst of 20000. The remote
// desktop relays carry the machine's screen out, which on a fast upload is
// 30 to 50 Mbit/s: 3000 to 5000 full-size packets a second, to one relay.
// 10000 pps is 120 Mbit/s of full-size packets, beyond any remote desktop,
// while a flood of small packets meets it at a few Mbit/s.
//
// Distinct hosts, 1000 a minute. A heavy page reaches 50 to 150 addresses
// (ads, analytics, CDNs); restoring a browser with many tabs can reach a
// few hundred. Scanning many hosts is mostly caught sooner by the
// unanswered-SYN count (most hosts do not answer on a given port), so this
// one can be generous and still stop a sweep of responsive hosts within
// seconds.
//
// Ports of one host, 100 a minute. Normal work uses one to a few ports per
// host (443, 80, 22, a call's media ports, a relay's few ports). Passive
// FTP opens a port per file, and ICE tries a peer's handful of candidates;
// both stay under 100 except an FTP mirror of many small files. A port scan
// (nmap's default is the top 1000 ports) passes 100 within a second.
//
// Unanswered SYNs, 300 a minute. An attempt counts when the far end refused
// it or did not answer in 35 s (the network's own dial gives up at 30 s; it
// answers the machine's SYN only once the real connection is made). A
// program retrying a dead server makes a few a second at most, and backs
// off; a scan of a thousand hosts or a flood of a silent one makes
// hundreds within seconds.
//
// Sustained, 30 s: a soft limit exceeded in every few seconds for half a
// minute is not a burst.
var defaultAbuse = abuseLimits{
	NewFlowsPerSec: 60,
	NewFlowsBurst:  200,
	UDPPerSec:      2000,
	UDPBurst:       4000,
	UDPAllPerSec:   10000,
	UDPAllBurst:    20000,
	DistinctIPs:    1000,
	PortsPerIP:     100,
	UnansweredSYNs: 300,
	SustainedSec:   30,
}

const (
	abuseWindow = 60 * time.Second
	// A UDP destination that sent something back this recently is answered.
	answeredFor = 10 * time.Second
	// How long an outbound SYN may wait for its SYN-ACK.
	synTimeout = 35 * time.Second
	// A soft limit counts as exceeded without pause while drops are no
	// further apart than this.
	breachGap = 2 * time.Second

	// Table sizes. A full table is far past the thresholds (that many hosts
	// or flows in a minute, or attempts in flight), so evicting an entry
	// to make room loses nothing that matters.
	maxIPs     = 8192
	maxPairs   = 16384
	maxPending = 4096
	maxRing    = 4096
)

// effective returns the limits with defaults filled in. Disabled checks
// (negative) come back as 0. Thresholds are kept below the table sizes, so
// that a full table can still trip them.
func (l abuseLimits) effective() abuseLimits {
	pickF := func(v, d float64) float64 {
		switch {
		case v < 0:
			return 0
		case v == 0:
			return d
		}
		return v
	}
	pickI := func(v, d, ceiling int) int {
		switch {
		case v < 0:
			return 0
		case v == 0:
			return d
		}
		return min(v, ceiling)
	}
	d := defaultAbuse
	return abuseLimits{
		Off:            l.Off,
		NewFlowsPerSec: pickF(l.NewFlowsPerSec, d.NewFlowsPerSec),
		NewFlowsBurst:  pickF(l.NewFlowsBurst, d.NewFlowsBurst),
		UDPPerSec:      pickF(l.UDPPerSec, d.UDPPerSec),
		UDPBurst:       pickF(l.UDPBurst, d.UDPBurst),
		UDPAllPerSec:   pickF(l.UDPAllPerSec, d.UDPAllPerSec),
		UDPAllBurst:    pickF(l.UDPAllBurst, d.UDPAllBurst),
		DistinctIPs:    pickI(l.DistinctIPs, d.DistinctIPs, maxIPs/2),
		PortsPerIP:     pickI(l.PortsPerIP, d.PortsPerIP, maxPairs/2),
		UnansweredSYNs: pickI(l.UnansweredSYNs, d.UnansweredSYNs, maxRing-1),
		SustainedSec:   pickF(l.SustainedSec, d.SustainedSec),
	}
}

// abuseEvent is what is reported when the network is cut for abuse.
type abuseEvent struct {
	Event  string `json:"event"` // always "abuse"
	Reason string `json:"reason"`
	Detail string `json:"detail"`
	Time   string `json:"time"`
}

// The reasons.
const (
	reasonFanOut       = "fan_out"
	reasonPortScan     = "port_scan"
	reasonHalfOpen     = "half_open"
	reasonSustainedNew = "sustained_new_flows"
	reasonSustainedUDP = "sustained_udp"
)

// rate is a token bucket counting events; a zero rate is no limit.
type rate struct {
	perSec, burst, tokens float64
	last                  int64
}

func (r *rate) set(perSec, burst float64) {
	r.perSec, r.burst = perSec, max(burst, 1)
	if r.last == 0 {
		r.tokens = r.burst
	}
	r.tokens = min(r.tokens, r.burst)
}

func (r *rate) refill(now int64) {
	if r.last != 0 && now > r.last {
		r.tokens = min(r.burst, r.tokens+float64(now-r.last)/1e9*r.perSec)
	}
	if now > r.last {
		r.last = now
	}
}

func (r *rate) room(now int64) bool {
	if r.perSec <= 0 {
		return true
	}
	r.refill(now)
	return r.tokens >= 1
}

func (r *rate) take() {
	if r.perSec > 0 {
		r.tokens--
	}
}

// streak follows how long a soft limit has been exceeded without pause.
type streak struct{ start, last, logged int64 }

// hit records a drop and returns how long the streak has lasted, and
// whether it just began.
func (s *streak) hit(now int64) (time.Duration, bool) {
	began := s.last == 0 || now-s.last > int64(breachGap)
	if began {
		s.start = now
	}
	s.last = now
	return time.Duration(now - s.start), began
}

// ring counts events in the last abuseWindow, exactly, in room for
// threshold+1 of them.
type ring struct {
	times []int64
	next  int
	n     int
}

func (r *ring) reset(threshold int) {
	r.times = make([]int64, threshold+1)
	r.next, r.n = 0, 0
}

// add records an event and reports whether more than the threshold fell in
// the window.
func (r *ring) add(now int64) bool {
	if len(r.times) <= 1 {
		return false // off
	}
	r.times[r.next] = now
	r.next = (r.next + 1) % len(r.times)
	if r.n < len(r.times) {
		r.n++
	}
	// Full: the oldest of threshold+1 events is the next slot.
	return r.n == len(r.times) && now-r.times[r.next] <= int64(abuseWindow)
}

type pairKey struct {
	ip    uint32
	port  uint16
	proto byte
}

type pairEntry struct {
	seen, reply int64
	// A UDP or ping flow that was let through as new; until then each of
	// its packets asks for a new-flow token.
	admitted bool
}

type synKey struct {
	ip           uint32
	port, source uint16
}

// abuseGuard watches what the machine sends. Its methods are called for
// every frame, from the goroutines reading and writing the machine's
// socket, so they are short, and most TCP frames take no lock at all.
type abuseGuard struct {
	gateway uint32
	// Times are kept as nanoseconds since epoch, on the monotonic clock,
	// so that the wall clock changing (NTP, the owner setting it) does not
	// expire or age everything at once.
	epoch   time.Time
	tripped atomic.Bool
	// report is called once, when the network is cut.
	report func(abuseEvent)

	// Frames dropped by the soft limits.
	droppedFlows, droppedUDP atomic.Uint64

	mu        sync.Mutex
	cfg       abuseLimits
	newFlows  rate
	udpQuiet  rate
	udpAll    rate
	ips       map[uint32]int64 // host -> last new flow to it
	pairs     map[pairKey]pairEntry
	portsOf   map[uint32]int // host -> its entries in pairs
	pending   map[synKey]int64
	noAnswer  ring
	flowRun   streak
	udpRun    streak
	lastSweep int64
}

func newAbuseGuard(gateway [4]byte, report func(abuseEvent)) *abuseGuard {
	g := &abuseGuard{
		gateway: binary.BigEndian.Uint32(gateway[:]),
		epoch:   time.Now(),
		report:  report,
		ips:     map[uint32]int64{},
		pairs:   map[pairKey]pairEntry{},
		portsOf: map[uint32]int{},
		pending: map[synKey]int64{},
	}
	g.set(abuseLimits{})
	return g
}

// set applies new limits, keeping what was counted so far.
func (g *abuseGuard) set(l abuseLimits) {
	g.mu.Lock()
	defer g.mu.Unlock()
	e := l.effective()
	if g.cfg.UnansweredSYNs != e.UnansweredSYNs || g.noAnswer.times == nil {
		g.noAnswer.reset(e.UnansweredSYNs)
	}
	g.cfg = e
	g.newFlows.set(e.NewFlowsPerSec, e.NewFlowsBurst)
	g.udpQuiet.set(e.UDPPerSec, e.UDPBurst)
	g.udpAll.set(e.UDPAllPerSec, e.UDPAllBurst)
}

// clock turns now into the guard's time, which starts an hour in, so that
// the zero of a field never yet set is long past.
func (g *abuseGuard) clock(now time.Time) int64 { return int64(now.Sub(g.epoch) + time.Hour) }

// isTripped is nil-safe, for a filteredConn without a guard.
func (g *abuseGuard) isTripped() bool { return g != nil && g.tripped.Load() }

// trip cuts the network, once. Called with mu held.
func (g *abuseGuard) trip(reason, detail string) {
	if g.tripped.Swap(true) {
		return
	}
	e := abuseEvent{Event: "abuse", Reason: reason, Detail: detail, Time: time.Now().UTC().Format(time.RFC3339)}
	log.Printf("network cut for abuse: %s: %s", reason, detail)
	if g.report != nil {
		g.report(e)
	}
}

// packet is what the guard needs of an IPv4 frame.
type packet4 struct {
	proto        byte
	src, dst     uint32
	sport, dport uint16
	tcpFlags     byte
	icmpType     byte
	ports        bool // the first fragment, with the transport header
}

const (
	protoICMP = 1
	tcpSYN    = 0x02
	tcpRST    = 0x04
	tcpACK    = 0x10
	icmpEcho  = 8
)

func parse4(frame []byte) (p packet4, ok bool) {
	if len(frame) < 14+20 || binary.BigEndian.Uint16(frame[12:14]) != etherIPv4 {
		return p, false
	}
	ip := frame[14:]
	ihl := int(ip[0]&0x0f) * 4
	if ip[0]>>4 != 4 || ihl < 20 || len(ip) < ihl {
		return p, false
	}
	p.proto = ip[9]
	p.src = binary.BigEndian.Uint32(ip[12:16])
	p.dst = binary.BigEndian.Uint32(ip[16:20])
	if binary.BigEndian.Uint16(ip[6:8])&0x1fff != 0 {
		return p, true
	}
	t := ip[ihl:]
	switch p.proto {
	case protoTCP:
		if len(t) >= 14 {
			p.sport, p.dport, p.tcpFlags, p.ports = binary.BigEndian.Uint16(t), binary.BigEndian.Uint16(t[2:]), t[13], true
		}
	case protoUDP:
		if len(t) >= 4 {
			p.sport, p.dport, p.ports = binary.BigEndian.Uint16(t), binary.BigEndian.Uint16(t[2:]), true
		}
	case protoICMP:
		if len(t) >= 1 {
			p.icmpType, p.ports = t[0], true
		}
	}
	return p, true
}

// out looks at a frame the machine sends and reports whether it may go on.
// It is called only for frames the policy allows.
func (g *abuseGuard) out(frame []byte, now time.Time) bool {
	if g == nil {
		return true
	}
	if g.tripped.Load() {
		return false
	}
	p, ok := parse4(frame)
	if !ok || p.dst == g.gateway || p.dst == 0xffffffff {
		return true
	}
	switch p.proto {
	case protoTCP:
		// Only connection attempts; the rest of TCP takes no lock.
		if !p.ports || p.tcpFlags&(tcpSYN|tcpACK|tcpRST) != tcpSYN {
			return true
		}
	case protoUDP:
	case protoICMP:
		if p.ports && p.icmpType != icmpEcho {
			return true
		}
	default:
		return true
	}
	t := g.clock(now)
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.cfg.Off {
		return true
	}
	g.sweep(t)
	if p.proto == protoTCP {
		return g.syn(p, t)
	}
	return g.datagram(p, t)
}

func (g *abuseGuard) syn(p packet4, now int64) bool {
	g.contact(p.dst, p.dport, protoTCP, now)
	if g.tripped.Load() {
		return false
	}
	k := synKey{p.dst, p.dport, p.sport}
	if _, again := g.pending[k]; again {
		return true // the machine sending the same SYN again
	}
	if !g.newFlows.room(now) {
		g.droppedFlows.Add(1)
		g.breach(&g.flowRun, reasonSustainedNew, "new connections and flows over %.0f a second", g.cfg.NewFlowsPerSec, now)
		return false
	}
	g.newFlows.take()
	if len(g.pending) >= maxPending {
		// That many attempts in flight is a flood whatever happens to
		// them: one makes room, counted as unanswered.
		for old := range g.pending {
			delete(g.pending, old)
			g.unanswered(now, "evicted")
			break
		}
	}
	g.pending[k] = now
	return true
}

func (g *abuseGuard) datagram(p packet4, now int64) bool {
	port := p.dport
	if p.proto == protoICMP {
		port = 0
	}
	k := pairKey{p.dst, port, p.proto}
	e, known := g.pairs[k]
	if known && now-e.seen > int64(abuseWindow) {
		g.forget(k) // stale, not yet swept: a new flow
		known = false
	}
	if !known {
		g.contact(p.dst, port, p.proto, now)
		if g.tripped.Load() {
			return false
		}
		e = g.pairs[k]
	}
	answered := e.reply != 0 && now-e.reply <= int64(answeredFor)
	quietOK := answered || g.udpQuiet.room(now)
	if !quietOK || !g.udpAll.room(now) {
		g.droppedUDP.Add(1)
		limit := g.cfg.UDPAllPerSec
		if !quietOK {
			limit = g.cfg.UDPPerSec
		}
		g.breach(&g.udpRun, reasonSustainedUDP, "UDP over %.0f packets a second", limit, now)
		return false
	}
	if !e.admitted {
		if !g.newFlows.room(now) {
			g.droppedFlows.Add(1)
			g.breach(&g.flowRun, reasonSustainedNew, "new connections and flows over %.0f a second", g.cfg.NewFlowsPerSec, now)
			return false
		}
		g.newFlows.take()
		e.admitted = true
	}
	if !answered {
		g.udpQuiet.take()
	}
	g.udpAll.take()
	e.seen = now
	g.pairs[k] = e
	return true
}

// contact records a new flow to ip:port (or an attempt at one) for the
// fan-out counts, and trips on a pattern.
func (g *abuseGuard) contact(ip uint32, port uint16, proto byte, now int64) {
	k := pairKey{ip, port, proto}
	if e, ok := g.pairs[k]; ok {
		e.seen = now
		g.pairs[k] = e
	} else {
		if len(g.pairs) >= maxPairs {
			for old := range g.pairs {
				g.forget(old)
				break
			}
		}
		g.pairs[k] = pairEntry{seen: now}
		g.portsOf[ip]++
		if n := g.cfg.PortsPerIP; n > 0 && g.portsOf[ip] > n {
			g.trip(reasonPortScan, fmt.Sprintf("more than %d ports of %s in a minute", n, ipString(ip)))
			return
		}
	}
	if _, ok := g.ips[ip]; !ok && len(g.ips) >= maxIPs {
		for old := range g.ips {
			delete(g.ips, old)
			break
		}
	}
	g.ips[ip] = now
	if n := g.cfg.DistinctIPs; n > 0 && len(g.ips) > n {
		g.trip(reasonFanOut, fmt.Sprintf("more than %d hosts in a minute", n))
	}
}

func (g *abuseGuard) forget(k pairKey) {
	if _, ok := g.pairs[k]; !ok {
		return
	}
	delete(g.pairs, k)
	if g.portsOf[k.ip]--; g.portsOf[k.ip] <= 0 {
		delete(g.portsOf, k.ip)
	}
}

func (g *abuseGuard) unanswered(now int64, why string) {
	if g.noAnswer.add(now) {
		g.trip(reasonHalfOpen, fmt.Sprintf("more than %d connection attempts unanswered in a minute (last %s)", g.cfg.UnansweredSYNs, why))
	}
}

func (g *abuseGuard) breach(s *streak, reason, format string, limit float64, now int64) {
	lasted, began := s.hit(now)
	if began && now-s.logged > int64(time.Minute) {
		s.logged = now
		log.Printf("abuse limits: dropping "+format, limit)
	}
	if sec := g.cfg.SustainedSec; sec > 0 && lasted >= time.Duration(sec*float64(time.Second)) {
		g.trip(reason, fmt.Sprintf(format+" for %.0f s", limit, sec))
	}
}

// tick sweeps the tables; main calls it every second, so that attempts
// left unanswered count even when the machine sends nothing more.
func (g *abuseGuard) tick(now time.Time) {
	if g == nil || g.tripped.Load() {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.cfg.Off {
		g.sweep(g.clock(now))
	}
}

// sweep forgets what fell out of the window, and counts SYNs that waited
// too long, at most once a second. Called with mu held.
func (g *abuseGuard) sweep(now int64) {
	if now-g.lastSweep < int64(time.Second) {
		return
	}
	g.lastSweep = now
	for ip, t := range g.ips {
		if now-t > int64(abuseWindow) {
			delete(g.ips, ip)
		}
	}
	for k, e := range g.pairs {
		if now-e.seen > int64(abuseWindow) {
			g.forget(k)
		}
	}
	for k, t := range g.pending {
		if now-t > int64(synTimeout) {
			delete(g.pending, k)
			g.unanswered(now, "timed out")
		}
	}
}

// in looks at a frame going to the machine: a SYN-ACK answers an attempt,
// a refusal (RST) leaves it unanswered, and any UDP from a destination
// marks it as answered.
func (g *abuseGuard) in(frame []byte, now time.Time) {
	if g == nil || g.tripped.Load() {
		return
	}
	p, ok := parse4(frame)
	if !ok || !p.ports || p.src == g.gateway {
		return
	}
	switch {
	case p.proto == protoTCP && (p.tcpFlags&(tcpSYN|tcpACK) == tcpSYN|tcpACK || p.tcpFlags&tcpRST != 0):
	case p.proto == protoUDP:
	default:
		return
	}
	t := g.clock(now)
	g.mu.Lock()
	defer g.mu.Unlock()
	if p.proto == protoUDP {
		k := pairKey{p.src, p.sport, protoUDP}
		if e, ok := g.pairs[k]; ok {
			e.reply = t
			g.pairs[k] = e
		}
		return
	}
	k := synKey{p.src, p.sport, p.dport}
	if _, ok := g.pending[k]; !ok {
		return
	}
	delete(g.pending, k)
	if p.tcpFlags&tcpRST != 0 {
		g.unanswered(t, "refused")
	}
}

func ipString(ip uint32) string {
	return fmt.Sprintf("%d.%d.%d.%d", byte(ip>>24), byte(ip>>16), byte(ip>>8), byte(ip))
}

// jsonLines writes events as JSON lines to w, one writer at a time.
type jsonLines struct {
	mu sync.Mutex
	w  io.Writer
}

func (j *jsonLines) write(v any) {
	j.mu.Lock()
	defer j.mu.Unlock()
	json.NewEncoder(j.w).Encode(v)
}
