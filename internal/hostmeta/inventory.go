package hostmeta

import "github.com/0intro/dd-micro-agent/internal/intake"

// The modern "inventory" metadata payloads, POSTed to /api/v1/metadata. Unlike the
// legacy v5 /intake/ payload (which lands the host in the Infrastructure List), these
// are what populate the host-detail page (the "host" and "Metrics" tabs) in a
// current Infrastructure UI. They mirror the stock Agent's comp/metadata/inventoryhost
// and comp/metadata/inventoryagent, and are pure JSON (no new dependencies). We fill
// the fields our existing per-OS gohai collectors already gather. The rest stay zero.

// inventoryHostPayload is the inventory_host payload.
type inventoryHostPayload struct {
	Hostname  string                `json:"hostname"`
	Timestamp int64                 `json:"timestamp"` // Unix nanoseconds
	Metadata  inventoryHostMetadata `json:"host_metadata"`
	UUID      string                `json:"uuid"`
}

// inventoryHostMetadata mirrors the stock Agent's host_metadata block.
type inventoryHostMetadata struct {
	CPUCores             uint64  `json:"cpu_cores"`
	CPULogicalProcessors uint64  `json:"cpu_logical_processors"`
	CPUVendor            string  `json:"cpu_vendor"`
	CPUModel             string  `json:"cpu_model"`
	CPUFrequency         float64 `json:"cpu_frequency"`
	KernelName           string  `json:"kernel_name"`
	KernelRelease        string  `json:"kernel_release"`
	KernelVersion        string  `json:"kernel_version"`
	OS                   string  `json:"os"`
	CPUArchitecture      string  `json:"cpu_architecture"`
	MemoryTotalKb        uint64  `json:"memory_total_kb"`
	MemorySwapTotalKb    uint64  `json:"memory_swap_total_kb"`
	IPAddress            string  `json:"ip_address"`
	IPv6Address          string  `json:"ipv6_address"`
	MacAddress           string  `json:"mac_address"`
	AgentVersion         string  `json:"agent_version"`
	OSVersion            string  `json:"os_version"`
}

// inventoryAgentPayload is the inventory_agent payload.
type inventoryAgentPayload struct {
	Hostname  string         `json:"hostname"`
	Timestamp int64          `json:"timestamp"`
	Metadata  map[string]any `json:"agent_metadata"`
	UUID      string         `json:"uuid"`
}

// inventoryHost builds the inventory_host payload from the already-collected gohai
// and systemStats (so the host isn't re-read).
func inventoryHost(hostname string, g gohai, s systemStats, nowNs int64) inventoryHostPayload {
	return inventoryHostPayload{
		Hostname:  hostname,
		Timestamp: nowNs,
		UUID:      hostUUID(hostname),
		Metadata: inventoryHostMetadata{
			CPUCores:             g.CPU.CPUCores,
			CPULogicalProcessors: g.CPU.CPULogicalProcessors,
			CPUVendor:            g.CPU.VendorID,
			CPUModel:             g.CPU.ModelName,
			CPUFrequency:         g.CPU.Mhz,
			KernelName:           g.Platform.KernelName,
			KernelRelease:        g.Platform.KernelRelease,
			KernelVersion:        g.Platform.KernelVersion,
			OS:                   g.Platform.OS,
			CPUArchitecture:      g.Platform.Machine,
			MemoryTotalKb:        g.Memory.Total / 1024, // gohai memory is bytes
			MemorySwapTotalKb:    g.Memory.SwapTotal,    // gohai swap is already kB
			IPAddress:            g.Network.IPAddress,
			IPv6Address:          g.Network.IPAddressV6,
			MacAddress:           g.Network.MacAddress,
			AgentVersion:         intake.Version,
			OSVersion:            osVersionString(s),
		},
	}
}

// inventoryAgent builds the inventory_agent payload.
func inventoryAgent(hostname string, nowNs int64) inventoryAgentPayload {
	return inventoryAgentPayload{
		Hostname:  hostname,
		Timestamp: nowNs,
		UUID:      hostUUID(hostname),
		Metadata: map[string]any{
			"agent_version": intake.Version,
			"flavor":        "dd-micro-agent",
			// The stock enum: full, end_user_device, basic, none. The backend
			// coerces anything else to full.
			"infrastructure_mode": "full",
		},
	}
}

// osVersionString returns the OS version from whichever per-OS array setOSVersion
// populated (nixV[1]/macV[0]/winV[1]).
func osVersionString(s systemStats) string {
	if v := verStr(s.NixV, 1); v != "" {
		return v
	}
	if v := verStr(s.MacV, 0); v != "" {
		return v
	}
	return verStr(s.WinV, 1)
}

// verStr returns element i of a version array when present and a plain string.
func verStr(v osVersion, i int) string {
	if i < len(v) {
		if s, ok := v[i].(string); ok {
			return s
		}
	}
	return ""
}
