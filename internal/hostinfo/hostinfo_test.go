package hostinfo

import (
	"runtime"
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
		if h := Collect(); h.GPUCores <= 0 {
			t.Errorf("no GPU cores read on this Mac: %+v", h)
		}
	}
}
