package requirements

import (
	"testing"

	"github.com/nodeexhq/miner/internal/hostspec"
)

func TestCheck(t *testing.T) {
	ok := hostspec.HostSpecs{Arch: "arm64", Chip: "Apple M4", CPUCores: 10, MemoryGB: 16, DiskTotalGB: 228, DiskFreeGB: 180, MacOS: "15.6"}
	if f := Check(ok); len(f) != 0 {
		t.Fatalf("baseline machine rejected: %+v", f)
	}
	cases := map[string]func(*hostspec.HostSpecs){
		"arch":       func(h *hostspec.HostSpecs) { h.Arch = "amd64" },
		"macos":      func(h *hostspec.HostSpecs) { h.MacOS = "14.7.1" },
		"cpu":        func(h *hostspec.HostSpecs) { h.CPUCores = 4 },
		"memory":     func(h *hostspec.HostSpecs) { h.MemoryGB = 8 },
		"disk_total": func(h *hostspec.HostSpecs) { h.DiskTotalGB = 113; h.DiskFreeGB = 100 },
		"disk_free":  func(h *hostspec.HostSpecs) { h.DiskFreeGB = 64 },
		"laptop":     func(h *hostspec.HostSpecs) { h.HasBattery = true },
	}
	for code, mutate := range cases {
		h := ok
		mutate(&h)
		f := Check(h)
		if len(f) != 1 || f[0].Code != code {
			t.Errorf("%s: got %+v", code, f)
		}
	}
	for _, v := range []string{"", "beta", "x.1"} {
		h := ok
		h.MacOS = v
		if f := Check(h); len(f) != 1 || f[0].Code != "macos" {
			t.Errorf("macOS %q: got %+v", v, f)
		}
	}
	// Newer major versions pass, and several failures are all reported.
	h := ok
	h.MacOS = "27.0.1"
	if f := Check(h); len(f) != 0 {
		t.Errorf("macOS 27 rejected: %+v", f)
	}
	if f := Check(hostspec.HostSpecs{}); len(f) != 6 {
		t.Errorf("empty specs: got %d failures, want 6: %+v", len(f), f)
	}
}
