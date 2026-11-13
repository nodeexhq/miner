// Package hostspec describes the hardware of a machine as the Miner reports
// it to the platform.
package hostspec

// HostSpecs is what the Miner measures on the machine it runs on. The JSON
// names are part of the platform's API.
type HostSpecs struct {
	Model       string `json:"model"`
	Arch        string `json:"arch"`
	Chip        string `json:"chip"`
	CPUCores    int    `json:"cpu_cores"`
	MemoryGB    int    `json:"memory_gb"`
	DiskTotalGB int    `json:"disk_total_gb"`
	DiskFreeGB  int    `json:"disk_free_gb"`
	MacOS       string `json:"macos"`
	HasBattery  bool   `json:"has_battery"`
}
