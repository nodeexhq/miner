package pretest

import (
	"errors"
	"testing"
	"time"
)

// The window formula exactly as the package documentation gives it, for
// Connect's port to match.
func TestFlowWindow(t *testing.T) {
	t0 := time.Unix(1_800_000_000, 0)
	ms := func(n int) time.Time { return t0.Add(time.Duration(n) * time.Millisecond) }
	f := &flow{rtt: 100 * time.Millisecond}
	if f.window() != WindowMin || !f.open() {
		t.Fatalf("initial window %d", f.window())
	}
	for f.open() {
		f.wrote(MaxData, t0)
	}
	if f.sent != WindowMin {
		t.Fatalf("sent %d before the window closed", f.sent)
	}
	// Less than an RTT after the first DATA: no rate yet.
	if err := f.ack(64<<10, ms(60)); err != nil || f.rate != 0 || f.window() != WindowMin {
		t.Fatalf("early ACK: rate %v window %d %v", f.rate, f.window(), err)
	}
	// An RTT after the first DATA: 128 KB in 100 ms.
	if err := f.ack(128<<10, ms(100)); err != nil {
		t.Fatal(err)
	}
	if want := float64(128<<10) / 0.1; f.rate != want || f.window() != 2*WindowMin {
		t.Errorf("rate %v (want %v), window %d", f.rate, want, f.window())
	}
	// The newest sample at least an RTT older is the base: (60 ms, 64 KB).
	for range 16 {
		f.wrote(MaxData, ms(100))
	}
	if err := f.ack(256<<10, ms(170)); err != nil {
		t.Fatal(err)
	}
	if want := float64(192<<10) / 0.11; f.rate != want {
		t.Errorf("rate %v, want %v", f.rate, want)
	}
	// A slower span does not lower it.
	before := f.rate
	f.ack(260<<10, ms(400))
	if f.rate != before {
		t.Errorf("rate fell to %v", f.rate)
	}
	// The cap.
	f.rate = 1e12
	if f.window() != WindowMax {
		t.Errorf("window %d over the cap", f.window())
	}
	// No RTT: the floor.
	if (&flow{rate: 1e9}).window() != WindowMin {
		t.Error("window without an RTT is not the floor")
	}

	if err := f.ack(f.sent+1, ms(500)); !errors.Is(err, ErrProtocol) {
		t.Errorf("ACK beyond sent: %v", err)
	}
	if err := f.ack(f.acked-1, ms(500)); !errors.Is(err, ErrProtocol) {
		t.Errorf("ACK going back: %v", err)
	}
}

// The receiver ACKs every AckEvery bytes.
func TestAcker(t *testing.T) {
	var a acker
	acks := 0
	for range 20 {
		if a.data(MaxData) {
			a.frame()
			acks++
		}
	}
	if acks != 20*MaxData/AckEvery || a.acked != a.received {
		t.Errorf("%d ACKs for %d bytes", acks, a.received)
	}
}
