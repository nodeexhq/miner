package hostinfo

import (
	"runtime"
	"strings"
	"testing"
)

func TestGPUCores(t *testing.T) {
	ioreg := `+-o AGXAcceleratorG16X  <class AGXAcceleratorG16X, id 0x100000a2c, registered, matched, active, busy 0 (0 ms), retain 47>
    {
      "IOClass" = "AGXAcceleratorG16X"
      "gpu-core-count" = 10
      "IOMatchCategory" = "IOAcceleratorES"
    }`
	for in, want := range map[string]int{ioreg: 10, "": 0, `"gpu-core-count" = 99999`: 0, `"gpu-core-count"=40`: 40} {
		if got := gpuCores(in); got != want {
			t.Errorf("gpuCores(%.40q) = %d, want %d", in, got, want)
		}
	}
	if runtime.GOOS == "darwin" && runtime.GOARCH == "arm64" {
		// A virtual Mac (such as a CI runner, model VirtualMac2,1) has no
		// GPU of its own to count; nodes are real Macs.
		if h := Collect(); h.GPUCores <= 0 && !strings.HasPrefix(h.Model, "VirtualMac") {
			t.Errorf("no GPU cores read on this Mac: %+v", h)
		}
	}
}
