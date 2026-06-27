//go:build windows

package process

// Windows process collection, no cgo. A Toolhelp snapshot lists every process
// (pid, ppid, name, thread count). Opening each process then yields its full
// image path, CPU times, create time, working set, handle count, and user.
// Memory and handle counts aren't exported by x/sys/windows, so we call them
// through lazily-loaded system DLLs, still pure Go. Windows has no process
// "state" (a listed process is running) and no POSIX uid/gid. The command line
// (behind the PEB) is out of scope for v1, so Args falls back to the image path.

import (
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	modpsapi                  = windows.NewLazySystemDLL("psapi.dll")
	procGetProcessMemoryInfo  = modpsapi.NewProc("GetProcessMemoryInfo")
	modkernel32               = windows.NewLazySystemDLL("kernel32.dll")
	procGetProcessHandleCount = modkernel32.NewProc("GetProcessHandleCount")
	procGlobalMemoryStatusEx  = modkernel32.NewProc("GlobalMemoryStatusEx")
)

// memoryStatusEx mirrors MEMORYSTATUSEX (sysinfoapi.h).
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

// hostTotalMemory returns total physical RAM in bytes via GlobalMemoryStatusEx.
// 0 if the call fails.
func hostTotalMemory() int64 {
	var ms memoryStatusEx
	ms.length = uint32(unsafe.Sizeof(ms))
	if r, _, _ := procGlobalMemoryStatusEx.Call(uintptr(unsafe.Pointer(&ms))); r == 0 {
		return 0
	}
	return int64(ms.totalPhys)
}

// processMemoryCounters mirrors PROCESS_MEMORY_COUNTERS (psapi.h).
type processMemoryCounters struct {
	cb                         uint32
	pageFaultCount             uint32
	peakWorkingSetSize         uintptr
	workingSetSize             uintptr
	quotaPeakPagedPoolUsage    uintptr
	quotaPagedPoolUsage        uintptr
	quotaPeakNonPagedPoolUsage uintptr
	quotaNonPagedPoolUsage     uintptr
	pagefileUsage              uintptr
	peakPagefileUsage          uintptr
}

func collectProcs() ([]Proc, error) {
	snap, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return nil, err
	}
	defer windows.CloseHandle(snap)

	var pe windows.ProcessEntry32
	pe.Size = uint32(unsafe.Sizeof(pe))
	if err := windows.Process32First(snap, &pe); err != nil {
		return nil, err
	}
	users := map[string]string{} // SID string -> account name
	var out []Proc
	for {
		if pe.ProcessID != 0 { // skip the System Idle Process
			p := Proc{
				Pid:     int32(pe.ProcessID),
				NsPid:   int32(pe.ProcessID),
				Ppid:    int32(pe.ParentProcessID),
				Name:    windows.UTF16ToString(pe.ExeFile[:]),
				Threads: int32(pe.Threads),
				State:   stateR,
			}
			p.Exe = p.Name
			enrich(&p, users)
			if len(p.Args) == 0 {
				p.Args = []string{p.Exe}
			}
			out = append(out, p)
		}
		if err := windows.Process32Next(snap, &pe); err != nil {
			break // ERROR_NO_MORE_FILES
		}
	}
	return out, nil
}

// enrich fills the fields Toolhelp doesn't carry. Every step is best-effort:
// unprivileged callers are denied access to system/protected processes.
func enrich(p *Proc, users map[string]string) {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(p.Pid))
	if err != nil {
		return
	}
	defer windows.CloseHandle(h)

	var creation, exit, kernel, user windows.Filetime
	if windows.GetProcessTimes(h, &creation, &exit, &kernel, &user) == nil {
		p.CreateTime = creation.Nanoseconds() / 1e6
		p.UserTime = filetimeSeconds(user)
		p.SystemTime = filetimeSeconds(kernel)
	}
	buf := make([]uint16, windows.MAX_PATH)
	size := uint32(len(buf))
	if windows.QueryFullProcessImageName(h, 0, &buf[0], &size) == nil {
		p.Exe = windows.UTF16ToString(buf[:size])
	}
	var pmc processMemoryCounters
	pmc.cb = uint32(unsafe.Sizeof(pmc))
	if r, _, _ := procGetProcessMemoryInfo.Call(uintptr(h), uintptr(unsafe.Pointer(&pmc)), uintptr(pmc.cb)); r != 0 {
		p.Rss = uint64(pmc.workingSetSize)
		p.Vms = uint64(pmc.pagefileUsage)
	}
	var count uint32
	if r, _, _ := procGetProcessHandleCount.Call(uintptr(h), uintptr(unsafe.Pointer(&count))); r != 0 {
		p.OpenFd = int32(count)
	}
	p.User = processUser(h, users)
}

// filetimeSeconds converts a FILETIME duration (100-ns ticks) to seconds. (For a
// wall-clock FILETIME use Filetime.Nanoseconds, which offsets from the 1601 epoch.)
func filetimeSeconds(ft windows.Filetime) float64 {
	return float64(uint64(ft.HighDateTime)<<32|uint64(ft.LowDateTime)) / 1e7
}

func processUser(h windows.Handle, cache map[string]string) string {
	var token windows.Token
	if windows.OpenProcessToken(h, windows.TOKEN_QUERY, &token) != nil {
		return ""
	}
	defer token.Close()
	tu, err := token.GetTokenUser()
	if err != nil {
		return ""
	}
	sid := tu.User.Sid.String()
	if name, ok := cache[sid]; ok {
		return name
	}
	name := ""
	if account, domain, _, err := tu.User.Sid.LookupAccount(""); err == nil {
		if domain != "" {
			name = domain + "\\" + account
		} else {
			name = account
		}
	}
	cache[sid] = name
	return name
}
