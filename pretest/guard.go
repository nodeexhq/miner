package pretest

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/nodeexhq/miner/leasetoken"
)

// BudgetWindow is the rolling window of the Miner's test budget,
// leasetoken.MinerHourlyTestBytes for all tests together, both ways.
const BudgetWindow = time.Hour

var (
	// ErrTokenUsed: the token's id was accepted before, and the token has
	// not expired yet.
	ErrTokenUsed = errors.New("pretest: test token already used")
	// ErrBusy: a lease is active on the node, a test is running, the
	// hour's budget is short, or the Miner cannot record the token's use.
	// The token is not spent; the client may try again.
	ErrBusy = errors.New("pretest: node is busy")
)

// Guard decides which tests the Miner admits: each token once, one test at
// a time, none during a lease, and no more than the budget. What it must
// remember across a restart (the token ids it accepted, until they expire,
// and what tests cost in the last hour) is kept in a small file.
type Guard struct {
	path        string
	leaseActive func() bool
	now         func() time.Time

	mu      sync.Mutex
	running bool
	seen    map[string]int64 // token id -> exp, Unix seconds
	spent   []spend
	// closedUntil is set when the file could not be read: the ids in it
	// are unknown, so no test is admitted until any token that could have
	// been among them has expired.
	closedUntil time.Time
}

type spend struct {
	At    int64 `json:"at"` // Unix seconds
	Bytes int64 `json:"bytes"`
}

type guardFile struct {
	Seen  map[string]int64 `json:"seen"`
	Spent []spend          `json:"spent"`
}

// OpenGuard loads the guard's file at path (none is a fresh start).
// leaseActive reports whether a lease is active on the node; now is the
// clock (nil for the real one). If the file is unreadable OpenGuard returns
// a working guard that admits no test for leasetoken.TestTTL, and the error.
func OpenGuard(path string, leaseActive func() bool, now func() time.Time) (*Guard, error) {
	if now == nil {
		now = time.Now
	}
	g := &Guard{path: path, leaseActive: leaseActive, now: now, seen: map[string]int64{}}
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return g, nil
	}
	var f guardFile
	if err == nil {
		err = json.Unmarshal(b, &f)
	}
	if err != nil {
		g.closedUntil = now().Add(leasetoken.TestTTL)
		return g, fmt.Errorf("pretest: cannot read %s, no tests for %s: %w", path, leasetoken.TestTTL, err)
	}
	if f.Seen != nil {
		g.seen = f.Seen
	}
	g.spent = f.Spent
	return g, nil
}

// Ticket is an admitted test. Done must be called when it ends.
type Ticket struct {
	g      *Guard
	Limits leasetoken.TestLimits // clamped
	once   sync.Once
}

// Admit admits the test of a verified test token (see
// leasetoken.VerifyTest), or says why not: ErrTokenUsed or ErrBusy.
func (g *Guard) Admit(c leasetoken.Claims) (*Ticket, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	now := g.now()
	g.pruneLocked(now)
	if c.TokenID == "" || c.Test == nil {
		return nil, leasetoken.ErrMalformed
	}
	if _, used := g.seen[c.TokenID]; used {
		return nil, ErrTokenUsed
	}
	switch {
	case now.Before(g.closedUntil):
		return nil, fmt.Errorf("%w: test state was lost, closed until %s", ErrBusy, g.closedUntil.Format(time.TimeOnly))
	case g.leaseActive != nil && g.leaseActive():
		return nil, fmt.Errorf("%w: a lease is active", ErrBusy)
	case g.running:
		return nil, fmt.Errorf("%w: a test is running", ErrBusy)
	}
	lim := c.Test.Clamp()
	if need, left := lim.MaxDownBytes+lim.MaxUpBytes, g.remainingLocked(); left < need {
		return nil, fmt.Errorf("%w: %d bytes left of the hour's test budget, test needs %d", ErrBusy, left, need)
	}
	g.seen[c.TokenID] = c.ExpiresAt
	// The id must be on disk before the test starts, or a restart could
	// admit the token again.
	if err := g.saveLocked(); err != nil {
		delete(g.seen, c.TokenID)
		return nil, fmt.Errorf("%w: %v", ErrBusy, err)
	}
	g.running = true
	return &Ticket{g: g, Limits: lim}, nil
}

// Done ends the test, charging the bytes it moved to the budget.
func (t *Ticket) Done(moved int64) error {
	var err error
	t.once.Do(func() {
		g := t.g
		g.mu.Lock()
		defer g.mu.Unlock()
		g.running = false
		g.spent = append(g.spent, spend{At: g.now().Unix(), Bytes: max(moved, 0)})
		err = g.saveLocked()
	})
	return err
}

// Remaining is what is left of the rolling hour's test budget.
func (g *Guard) Remaining() int64 {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.pruneLocked(g.now())
	return g.remainingLocked()
}

func (g *Guard) remainingLocked() int64 {
	left := int64(leasetoken.MinerHourlyTestBytes)
	for _, s := range g.spent {
		left -= s.Bytes
	}
	return left
}

// pruneLocked forgets token ids whose tokens have expired (verification
// refuses them anyway) and spending older than the budget's window.
func (g *Guard) pruneLocked(now time.Time) {
	for id, exp := range g.seen {
		if now.Unix() >= exp {
			delete(g.seen, id)
		}
	}
	cut := now.Add(-BudgetWindow).Unix()
	kept := g.spent[:0]
	for _, s := range g.spent {
		if s.At > cut {
			kept = append(kept, s)
		}
	}
	g.spent = kept
}

func (g *Guard) saveLocked() error {
	if g.path == "" {
		return nil
	}
	b, err := json.Marshal(guardFile{Seen: g.seen, Spent: g.spent})
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(g.path), ".pretest-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), g.path)
}
