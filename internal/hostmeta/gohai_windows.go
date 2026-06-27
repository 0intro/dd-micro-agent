package hostmeta

import (
	"runtime"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

// collectGohai gathers host facts on Windows via the registry and Win32 APIs
// (pure Go, no cgo). Filesystem enumeration is skipped (untested here).
func collectGohai(hostname string) gohai {
	return gohai{
		Platform: gohaiPlatform{
			GoVersion:     runtime.Version(),
			GoOS:          runtime.GOOS,
			GoArch:        runtime.GOARCH,
			KernelName:    "Windows",
			KernelRelease: winBuild(),
			Hostname:      hostname,
			Machine:       runtime.GOARCH,
			Processor:     runtime.GOARCH,
			OS:            winProductName(),
		},
		CPU: gohaiCPU{
			ModelName: winCPUModel(),
			// cpu_cores carries the logical processor count on Windows, since
			// counting physical cores needs GetLogicalProcessorInformationEx.
			CPUCores:             uint64(runtime.NumCPU()),
			CPULogicalProcessors: uint64(runtime.NumCPU()),
		},
		Memory:     gohaiMemory{Total: winMemTotal()},
		Network:    collectNetwork(),
		FileSystem: []gohaiFS{},
	}
}

// setOSVersion fills winV = [product name, build], which is what makes the
// backend show Windows and its icon. The stock's Windows osVersion is a
// [2]string, so all four arrays are two elements here.
func setOSVersion(s *systemStats) {
	s.MacV, s.NixV, s.FbsdV = osVersion{"", ""}, osVersion{"", ""}, osVersion{"", ""}
	s.WinV = osVersion{winProductName(), winBuild()}
}

func winProductName() string {
	return regString(`SOFTWARE\Microsoft\Windows NT\CurrentVersion`, "ProductName")
}

func winBuild() string {
	return regString(`SOFTWARE\Microsoft\Windows NT\CurrentVersion`, "CurrentBuild")
}

func winCPUModel() string {
	return regString(`HARDWARE\DESCRIPTION\System\CentralProcessor\0`, "ProcessorNameString")
}

func regString(path, name string) string {
	k, err := registry.OpenKey(registry.LOCAL_MACHINE, path, registry.QUERY_VALUE)
	if err != nil {
		return ""
	}
	defer k.Close()
	v, _, err := k.GetStringValue(name)
	if err != nil {
		return ""
	}
	return v
}

type memoryStatusEx struct {
	length               uint32
	memoryLoad           uint32
	totalPhys            uint64
	availPhys            uint64
	totalPageFile        uint64
	availPageFile        uint64
	totalVirtual         uint64
	availVirtual         uint64
	availExtendedVirtual uint64
}

var procGlobalMemoryStatusEx = windows.NewLazySystemDLL("kernel32.dll").NewProc("GlobalMemoryStatusEx")

func winMemTotal() uint64 {
	var m memoryStatusEx
	m.length = uint32(unsafe.Sizeof(m))
	if r, _, _ := procGlobalMemoryStatusEx.Call(uintptr(unsafe.Pointer(&m))); r == 0 {
		return 0
	}
	return m.totalPhys
}
