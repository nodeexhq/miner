// Package hostinfo collects the machine facts a Miner reports.
package hostinfo

import (
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"syscall"

	"github.com/nodeexhq/miner/internal/hostspec"
)

const gib = 1 << 30

// Collect reads the host's specs. Fields it cannot determine are left at
// their runtime-derived defaults.
func Collect() hostspec.HostSpecs {
	h := hostspec.HostSpecs{CPUCores: runtime.NumCPU(), Arch: runtime.GOARCH, Chip: runtime.GOARCH}
	if runtime.GOOS == "darwin" {
		h.Model = sysctl("hw.model")
		// Report the hardware, not the binary: an amd64 build under Rosetta
		// still runs on Apple silicon.
		if sysctl("hw.optional.arm64") == "1" {
			h.Arch = "arm64"
		}
		h.HasBattery = strings.Contains(output("pmset", "-g", "batt"), "InternalBattery")
		if chip := sysctl("machdep.cpu.brand_string"); chip != "" {
			h.Chip = chip
		}
		if mem, err := strconv.ParseUint(sysctl("hw.memsize"), 10, 64); err == nil {
			h.MemoryGB = int((mem + gib/2) / gib)
		}
		h.MacOS = output("sw_vers", "-productVersion")
	}
	var fs syscall.Statfs_t
	// The data volume is where VM images live; "/" is the sealed system volume.
	path := "/"
	if runtime.GOOS == "darwin" {
		path = "/System/Volumes/Data"
	}
	if err := syscall.Statfs(path, &fs); err == nil {
		h.DiskTotalGB = int(fs.Blocks * uint64(fs.Bsize) / gib)
		h.DiskFreeGB = int(fs.Bavail * uint64(fs.Bsize) / gib)
	}
	return h
}

func sysctl(name string) string { return output("sysctl", "-n", name) }

func output(name string, args ...string) string {
	out, err := exec.Command(name, args...).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// Load1 is the one-minute load average, or 0 if it cannot be read.
func Load1() float64 {
	var text string
	if runtime.GOOS == "darwin" {
		// Prints "{ 1.25 1.30 1.34 }".
		text = strings.Trim(sysctl("vm.loadavg"), "{} ")
	} else if b, err := os.ReadFile("/proc/loadavg"); err == nil {
		text = string(b)
	}
	fields := strings.Fields(text)
	if len(fields) == 0 {
		return 0
	}
	v, err := strconv.ParseFloat(fields[0], 64)
	if err != nil || v < 0 {
		return 0
	}
	return v
}
