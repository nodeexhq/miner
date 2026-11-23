package pretest

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/nvtohq/node/leasetoken"
)

type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }

func testClaims(now time.Time, lim leasetoken.TestLimits) leasetoken.Claims {
	return leasetoken.Claims{
		NodeID: "node-1", Services: []string{leasetoken.ServiceTest}, Scope: leasetoken.ScopeTest,
		TokenID: leasetoken.NewTokenID(), ReservationID: "res-1",
		IssuedAt: now.Unix(), ExpiresAt: now.Add(leasetoken.TestTTL).Unix(), Test: &lim,
	}
}

func newGuard(t *testing.T, path string, c *clock, leased *bool) *Guard {
	t.Helper()
	g, err := OpenGuard(path, func() bool { return *leased }, c.now)
	if err != nil {
		t.Fatal(err)
	}
	return g
}

func TestGuardSingleUse(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pretest.json")
	c := &clock{time.Unix(1_800_000_000, 0)}
	leased := false
	g := newGuard(t, path, c, &leased)
	a := testClaims(c.t, leasetoken.DefaultTestLimits())

	tk, err := g.Admit(a)
	if err != nil {
		t.Fatal(err)
	}
	tk.Done(1000)
	if _, err := g.Admit(a); !errors.Is(err, ErrTokenUsed) {
		t.Errorf("second use: %v", err)
	}

	// A restart within the token's life does not forget it.
	c.t = c.t.Add(leasetoken.TestTTL - time.Second)
	g = newGuard(t, path, c, &leased)
	if _, err := g.Admit(a); !errors.Is(err, ErrTokenUsed) {
		t.Errorf("replay after a restart: %v", err)
	}
	if g.Remaining() != leasetoken.NodeHourlyTestBytes-1000 {
		t.Errorf("budget after a restart: %d left", g.Remaining())
	}

	// Once the token has expired its id is dropped from the file (the
	// gate refuses the token as expired anyway).
	c.t = c.t.Add(time.Second)
	b := testClaims(c.t, leasetoken.DefaultTestLimits())
	tk, err = g.Admit(b)
	if err != nil {
		t.Fatal(err)
	}
	tk.Done(0)
	g = newGuard(t, path, c, &leased)
	if _, ok := g.seen[a.TokenID]; ok || len(g.seen) != 1 {
		t.Errorf("seen after expiry: %v", g.seen)
	}
}

func TestGuardBusy(t *testing.T) {
	c := &clock{time.Unix(1_800_000_000, 0)}
	leased := true
	g := newGuard(t, filepath.Join(t.TempDir(), "pretest.json"), c, &leased)
	a := testClaims(c.t, leasetoken.DefaultTestLimits())

	if _, err := g.Admit(a); !errors.Is(err, ErrBusy) {
		t.Errorf("during a lease: %v", err)
	}
	// Refusing as busy does not spend the token.
	leased = false
	tk, err := g.Admit(a)
	if err != nil {
		t.Fatalf("after the lease: %v", err)
	}
	b := testClaims(c.t, leasetoken.DefaultTestLimits())
	if _, err := g.Admit(b); !errors.Is(err, ErrBusy) {
		t.Errorf("during another test: %v", err)
	}
	tk.Done(0)
	tk.Done(5_000_000) // a second Done is ignored
	if tk, err := g.Admit(b); err != nil {
		t.Errorf("after the other test: %v", err)
	} else {
		tk.Done(0)
	}
	if g.Remaining() != leasetoken.NodeHourlyTestBytes {
		t.Errorf("budget charged twice for one test: %d left", g.Remaining())
	}
}

func TestGuardBudget(t *testing.T) {
	c := &clock{time.Unix(1_800_000_000, 0)}
	leased := false
	g := newGuard(t, filepath.Join(t.TempDir(), "pretest.json"), c, &leased)
	full := leasetoken.DefaultTestLimits() // 20 MB a test at most

	run := func(lim leasetoken.TestLimits, moved int64) error {
		tk, err := g.Admit(testClaims(c.t, lim))
		if err == nil {
			tk.Done(moved)
		}
		return err
	}
	// Nine full tests that each moved their 20 MB, a minute apart.
	for range 9 {
		if err := run(full, 20_000_000); err != nil {
			t.Fatal(err)
		}
		c.t = c.t.Add(time.Minute)
	}
	// What is charged is what was moved: a test that stopped early costs
	// little.
	if err := run(full, 1_000_000); err != nil {
		t.Fatal(err)
	}
	if g.Remaining() != 19_000_000 {
		t.Fatalf("%d left", g.Remaining())
	}
	// 19 MB left: not enough for a full test, enough for a smaller one.
	if err := run(full, 0); !errors.Is(err, ErrBusy) {
		t.Errorf("full test with 19 MB left: %v", err)
	}
	small := leasetoken.TestLimits{Pings: 10, MaxDownBytes: 9_000_000, MaxUpBytes: 9_000_000, MaxSeconds: 10}
	if err := run(small, 18_000_000); err != nil {
		t.Errorf("18 MB test with 19 MB left: %v", err)
	}
	// Limits above the node's maxima are clamped before the budget is
	// checked, so an inflated token is not refused, and not let through
	// at its own word.
	c.t = c.t.Add(BudgetWindow)
	huge := leasetoken.TestLimits{Pings: 1000, MaxDownBytes: 1 << 40, MaxUpBytes: 1 << 40, MaxSeconds: 3600}
	tk, err := g.Admit(testClaims(c.t, huge))
	if err != nil {
		t.Fatalf("an hour later: %v", err)
	}
	maxima := leasetoken.TestLimits{Pings: leasetoken.NodeMaxPings, MaxDownBytes: leasetoken.NodeMaxDownBytes,
		MaxUpBytes: leasetoken.NodeMaxUpBytes, MaxSeconds: leasetoken.NodeMaxSeconds}
	if tk.Limits != maxima {
		t.Errorf("clamped limits %+v", tk.Limits)
	}
	tk.Done(0)
	// An hour on, all of it has aged out.
	if left := g.Remaining(); left != leasetoken.NodeHourlyTestBytes {
		t.Errorf("an hour after the last test: %d left", left)
	}
}

func TestGuardRollingWindow(t *testing.T) {
	c := &clock{time.Unix(1_800_000_000, 0)}
	leased := false
	g := newGuard(t, "", c, &leased)
	for _, at := range []time.Duration{0, 30 * time.Minute} {
		c.t = time.Unix(1_800_000_000, 0).Add(at)
		tk, err := g.Admit(testClaims(c.t, leasetoken.DefaultTestLimits()))
		if err != nil {
			t.Fatal(err)
		}
		tk.Done(90_000_000)
	}
	if g.Remaining() != 20_000_000 {
		t.Errorf("%d left", g.Remaining())
	}
	c.t = time.Unix(1_800_000_000, 0).Add(BudgetWindow + time.Second)
	if g.Remaining() != 110_000_000 {
		t.Errorf("after the first test aged out: %d left", g.Remaining())
	}
}

// A state file that cannot be read may have held ids that are still live:
// no test until they must have expired.
func TestGuardLostState(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pretest.json")
	os.WriteFile(path, []byte("{not json"), 0o600)
	c := &clock{time.Unix(1_800_000_000, 0)}
	leased := false
	g, err := OpenGuard(path, func() bool { return leased }, c.now)
	if err == nil {
		t.Error("corrupt file not reported")
	}
	if _, err := g.Admit(testClaims(c.t, leasetoken.DefaultTestLimits())); !errors.Is(err, ErrBusy) {
		t.Errorf("with lost state: %v", err)
	}
	c.t = c.t.Add(leasetoken.TestTTL)
	if _, err := g.Admit(testClaims(c.t, leasetoken.DefaultTestLimits())); err != nil {
		t.Errorf("once any lost token has expired: %v", err)
	}

	// If the id cannot be recorded the test is not admitted.
	g = newGuard(t, filepath.Join(t.TempDir(), "missing-dir", "pretest.json"), c, &leased)
	a := testClaims(c.t, leasetoken.DefaultTestLimits())
	if _, err := g.Admit(a); !errors.Is(err, ErrBusy) {
		t.Errorf("unrecordable: %v", err)
	}
	if len(g.seen) != 0 || g.running {
		t.Errorf("state after a failed save: %v running=%v", g.seen, g.running)
	}
}
