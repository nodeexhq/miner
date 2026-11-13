// Package requirements defines the minimum hardware a machine needs to join
// the network as a node. The Miner checks it before enrolling so the owner
// gets a clear answer up front; the platform enforces it again on enrollment.
package requirements

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/nodeexhq/miner/internal/hostspec"
)

const (
	// Apple silicon is required: macOS guests only run under
	// Virtualization.framework on arm64.
	Arch = "arm64"
	// The host and guest both need macOS 15 for Apple ID sign-in inside
	// the VM, which renters need to install Xcode.
	MinMacOSMajor = 15
	MinCPUCores   = 8
	// The renter's VM gets everything except a small host reserve, so
	// anything under 16 GB leaves too little to be worth renting.
	MinMemoryGB = 16
	// Sized for one VM image plus its working clone. Capacities are in
	// GiB as the filesystem reports them: a "256 GB" disk shows about 228.
	MinDiskTotalGB = 220
	MinDiskFreeGB  = 100
)

// Failure is one unmet requirement.
type Failure struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// Check returns every requirement the host fails; an empty result means it
// may enroll.
func Check(h hostspec.HostSpecs) []Failure {
	var out []Failure
	fail := func(code, format string, args ...any) {
		out = append(out, Failure{code, fmt.Sprintf(format, args...)})
	}
	if h.Arch != Arch {
		fail("arch", "Apple silicon (M1 or later) is required; this machine is %q", h.Arch)
	}
	if major, ok := macOSMajor(h.MacOS); !ok || major < MinMacOSMajor {
		fail("macos", "macOS %d or later is required; this machine runs %q", MinMacOSMajor, h.MacOS)
	}
	if h.CPUCores < MinCPUCores {
		fail("cpu", "at least %d CPU cores are required; this machine has %d", MinCPUCores, h.CPUCores)
	}
	if h.MemoryGB < MinMemoryGB {
		fail("memory", "at least %d GB of memory is required; this machine has %d GB", MinMemoryGB, h.MemoryGB)
	}
	if h.DiskTotalGB < MinDiskTotalGB {
		fail("disk_total", "a disk of at least 256 GB is required; this machine has %d GB", h.DiskTotalGB)
	}
	if h.DiskFreeGB < MinDiskFreeGB {
		fail("disk_free", "at least %d GB of free disk space is required; this machine has %d GB free", MinDiskFreeGB, h.DiskFreeGB)
	}
	if h.HasBattery {
		fail("laptop", "laptops cannot be nodes: they sleep when closed and cannot stay online; use a Mac mini, Mac Studio or other desktop Mac")
	}
	return out
}

func macOSMajor(v string) (int, bool) {
	major, _, _ := strings.Cut(v, ".")
	n, err := strconv.Atoi(major)
	return n, err == nil
}
