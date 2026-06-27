//go:build darwin || freebsd || openbsd || netbsd || dragonfly

package hostmeta

import (
	"encoding/binary"
	"runtime"

	"golang.org/x/sys/unix"
)

// collectGohai gathers host facts via sysctl (and getfsstat for filesystems on
// the platforms that support our shared reader). Pure Go, no cgo.
func collectGohai(hostname string) gohai {
	model, cores, logical, mhz := unixCPU()
	return gohai{
		Platform: unixPlatform(hostname),
		CPU: gohaiCPU{
			ModelName:            model,
			CPUCores:             cores,
			CPULogicalProcessors: logical,
			Mhz:                  mhz,
		},
		Memory:     gohaiMemory{Total: unixMemTotal()},
		Network:    collectNetwork(),
		FileSystem: unixFilesystem(), // per-OS (see gohai_fs_*.go)
	}
}

// setOSVersion fills the systemStats OS-version array (drives the OS detail/icon):
// macV on darwin, nixV on the BSDs. nixV[0] must be the lowercase platform name
// the backend keys the icon on ("freebsd"/"openbsd"/"netbsd"/"dragonfly") (i.e.
// runtime.GOOS, matching gopsutil/the stock Agent), NOT kern.ostype ("FreeBSD"),
// which the backend doesn't recognize, leaving the host with no OS logo.
func setOSVersion(s *systemStats) {
	rel := sysctlStr("kern.osrelease")
	if runtime.GOOS == "darwin" {
		if pv := sysctlStr("kern.osproductversion"); pv != "" {
			rel = pv
		}
		// The stock macV is [version, ["","",""], arch], with a nested empty
		// three-string array (python mac_ver's versioninfo tuple) in the middle.
		s.MacV = osVersion{rel, [3]string{"", "", ""}, runtime.GOARCH}
		return
	}
	s.NixV = osVersion{runtime.GOOS, rel, ""}
}

func unixPlatform(hostname string) gohaiPlatform {
	machine := sysctlStr("hw.machine")
	if machine == "" {
		machine = runtime.GOARCH
	}
	osName := sysctlStr("kern.ostype") // "FreeBSD" / "OpenBSD" / "NetBSD" / "Darwin" / "DragonFly"
	return gohaiPlatform{
		GoVersion:     runtime.Version(),
		GoOS:          runtime.GOOS,
		GoArch:        runtime.GOARCH,
		KernelName:    sysctlStr("kern.ostype"),
		KernelRelease: sysctlStr("kern.osrelease"),
		KernelVersion: sysctlStr("kern.version"),
		Hostname:      hostname,
		Machine:       machine,
		Processor:     machine,
		OS:            osName,
	}
}

func unixCPU() (model string, cores, logical uint64, mhz float64) {
	logical = sysctlNum("hw.ncpu")
	cores = logical
	switch runtime.GOOS {
	case "darwin":
		model = sysctlStr("machdep.cpu.brand_string")
		if p := sysctlNum("hw.physicalcpu"); p > 0 {
			cores = p
		}
		if l := sysctlNum("hw.logicalcpu"); l > 0 {
			logical = l
		}
		if hz := sysctlNum("hw.cpufrequency"); hz > 0 {
			mhz = float64(hz) / 1e6
		}
	case "freebsd", "dragonfly": // same hw.model and hw.clockrate MIBs
		model = sysctlStr("hw.model")
		mhz = float64(sysctlNum("hw.clockrate"))
	case "openbsd":
		model = sysctlStr("hw.model")
		mhz = float64(sysctlNum("hw.cpuspeed"))
	default: // netbsd
		model = sysctlStr("hw.model")
	}
	return model, cores, logical, mhz
}

func unixMemTotal() uint64 {
	switch runtime.GOOS {
	case "darwin":
		return sysctlNum("hw.memsize")
	case "freebsd", "dragonfly": // hw.physmem on both
		return sysctlNum("hw.physmem")
	case "openbsd":
		// OpenBSD's sysctl name table omits hw.physmem64, so fall back to the
		// classic hw.physmem. The system.mem.total metric uses vm.uvmexp instead,
		// which is exact regardless of size (see host.openbsdMem).
		if t := sysctlNum("hw.physmem64"); t != 0 {
			return t
		}
		return sysctlNum("hw.physmem")
	default: // netbsd
		return sysctlNum("hw.physmem64")
	}
}

func sysctlStr(name string) string {
	v, err := unix.Sysctl(name)
	if err != nil {
		return ""
	}
	return v
}

// sysctlNum reads a numeric sysctl as raw bytes (uniform across BSDs) and decodes
// it as a little-endian 4- or 8-byte integer (our targets are little-endian).
func sysctlNum(name string) uint64 {
	b, err := unix.SysctlRaw(name)
	if err != nil {
		return 0
	}
	switch {
	case len(b) >= 8:
		return binary.LittleEndian.Uint64(b[:8])
	case len(b) >= 4:
		return uint64(binary.LittleEndian.Uint32(b[:4]))
	default:
		return 0
	}
}
