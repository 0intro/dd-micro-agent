package hostmeta

import (
	"os"
	"runtime"
	"strings"
)

// collectGohai gathers Plan 9 host facts from the kernel's text files under /dev.
// Pure file reads: no syscalls, no x/sys. Filesystem is left empty: Plan 9 has no
// statfs, and free space lives behind each file server's ctl.
func collectGohai(hostname string) gohai {
	model, mhz := parsePlan9Cputype(readDev("/dev/cputype"))
	ncpu := uint64(countPlan9CPUs(readDev("/dev/sysstat")))
	return gohai{
		Platform: plan9Platform(hostname, model),
		CPU: gohaiCPU{
			ModelName:            model,
			CPUCores:             ncpu,
			CPULogicalProcessors: ncpu,
			Mhz:                  mhz,
		},
		Memory:     gohaiMemory{Total: parsePlan9MemTotal(readDev("/dev/swap"))},
		Network:    collectNetwork(),
		FileSystem: []gohaiFS{},
	}
}

// setOSVersion fills nixV = ["plan9", version, ""]. The lowercase token in nixV[0]
// is what the backend keys the OS icon on. Like the BSDs, Datadog has no plan9 icon,
// but we send the correct value regardless. version is /dev/osversion when present,
// else "9legacy".
func setOSVersion(s *systemStats) {
	s.NixV = osVersion{"plan9", plan9Version(), ""}
}

func plan9Platform(hostname, model string) gohaiPlatform {
	return gohaiPlatform{
		GoVersion:     runtime.Version(),
		GoOS:          runtime.GOOS,
		GoArch:        runtime.GOARCH,
		KernelName:    "Plan 9",
		KernelRelease: plan9Version(),
		Hostname:      hostname,
		Machine:       runtime.GOARCH,
		Processor:     model,
		OS:            "plan9",
	}
}

func plan9Version() string {
	if v := strings.TrimSpace(readDev("/dev/osversion")); v != "" {
		return v
	}
	return "9legacy"
}

func readDev(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return string(b)
}
