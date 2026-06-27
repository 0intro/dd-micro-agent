//go:build linux

package process

// Linux process collection from /proc. Everything is a plain file read: no
// x/sys, no cgo. The format parsers (parseStat/parseStatus/...) are pure so they
// unit-test against fixtures. collectProcs walks the real /proc and is covered by
// one smoke test. CPU times are in clock ticks (USER_HZ, 100 on Linux). Memory
// from /proc/<pid>/status is in kB. rss in /proc/<pid>/stat is in pages.

import (
	"os"
	"strconv"
	"strings"
)

func collectProcs() ([]Proc, error) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, err
	}
	btime := readBtime("/proc/stat")
	pagesize := uint64(os.Getpagesize())
	users := map[int32]string{} // uid -> name, resolved once per collection
	out := make([]Proc, 0, len(entries))
	for _, e := range entries {
		pid, ok := parsePid(e.Name())
		if !ok {
			continue
		}
		if p, ok := readProc(pid, btime, pagesize, users); ok {
			out = append(out, p)
		}
	}
	return out, nil
}

func readProc(pid int32, btime, pagesize uint64, users map[int32]string) (Proc, bool) {
	base := "/proc/" + strconv.Itoa(int(pid))
	statData, err := os.ReadFile(base + "/stat")
	if err != nil {
		return Proc{}, false // exited between ReadDir and now: expected
	}
	st, ok := parseStat(string(statData))
	if !ok {
		return Proc{}, false
	}
	p := Proc{
		Pid: pid, Ppid: st.ppid, NsPid: pid,
		Name:       st.comm,
		State:      linuxState(st.state),
		Threads:    st.numThreads,
		UserTime:   float64(st.utime) / clockTicks,
		SystemTime: float64(st.stime) / clockTicks,
		Vms:        st.vsize,
		Rss:        st.rssPages * pagesize,
	}
	if btime > 0 {
		p.CreateTime = int64((float64(btime) + float64(st.starttime)/clockTicks) * 1000)
	}

	if data, err := os.ReadFile(base + "/status"); err == nil {
		s := parseStatus(string(data))
		if s.haveUID {
			p.Uid, p.Gid = s.uid, s.gid
			p.User = lookupUser(s.uid, users)
		}
		if s.vmRSS > 0 {
			p.Rss = s.vmRSS * 1024
		}
		if s.vmSize > 0 {
			p.Vms = s.vmSize * 1024
		}
		p.Swap = s.vmSwap * 1024
		if s.threads > 0 {
			p.Threads = s.threads
		}
		p.VoluntaryCtxSwitches = s.volctx
		p.InvoluntaryCtxSwitches = s.nonvolctx
	}
	if data, err := os.ReadFile(base + "/cmdline"); err == nil {
		if args := parseCmdline(data); len(args) > 0 {
			p.Args, p.Exe = args, args[0]
		}
	}
	if cwd, err := os.Readlink(base + "/cwd"); err == nil {
		p.Cwd = cwd
	}
	// io and fd need privilege for other users' processes. Skip on error.
	if data, err := os.ReadFile(base + "/io"); err == nil {
		parseIO(string(data), &p)
	}
	if fds, err := os.ReadDir(base + "/fd"); err == nil {
		p.OpenFd = int32(len(fds))
	}
	return p, true
}

// statLine is the slice of /proc/<pid>/stat we use.
type statLine struct {
	comm       string
	state      byte
	ppid       int32
	numThreads int32
	utime      uint64 // clock ticks
	stime      uint64 // clock ticks
	starttime  uint64 // clock ticks since boot
	vsize      uint64 // bytes
	rssPages   uint64 // pages
}

// parseStat parses /proc/<pid>/stat. comm is bracketed and may contain spaces and
// parentheses, so it is taken between the first '(' and the last ')'. The numeric
// fields follow. After the comm, field 3 (state) is index 0, so field N is index
// N-3: utime=14→11, stime=15→12, num_threads=20→17, starttime=22→19, vsize=23→20,
// rss=24→21.
func parseStat(data string) (statLine, bool) {
	open := strings.IndexByte(data, '(')
	closeP := strings.LastIndexByte(data, ')')
	if open < 0 || closeP < 0 || closeP < open {
		return statLine{}, false
	}
	f := strings.Fields(data[closeP+1:])
	if len(f) < 22 {
		return statLine{}, false
	}
	return statLine{
		comm:       data[open+1 : closeP],
		state:      f[0][0],
		ppid:       atoi32(f[1]),
		utime:      atou(f[11]),
		stime:      atou(f[12]),
		numThreads: atoi32(f[17]),
		starttime:  atou(f[19]),
		vsize:      atou(f[20]),
		rssPages:   atou(f[21]),
	}, true
}

type statusInfo struct {
	haveUID           bool
	uid, gid, threads int32
	vmRSS, vmSize     uint64 // kB
	vmSwap            uint64 // kB
	volctx, nonvolctx uint64
}

// parseStatus reads the "Key:\tvalue" lines of /proc/<pid>/status that we use.
func parseStatus(data string) statusInfo {
	var s statusInfo
	for _, line := range strings.Split(data, "\n") {
		key, val, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		val = strings.TrimSpace(val)
		switch key {
		case "Uid":
			s.uid, s.haveUID = firstFieldInt32(val), true
		case "Gid":
			s.gid = firstFieldInt32(val)
		case "VmRSS":
			s.vmRSS = firstFieldUint(val)
		case "VmSize":
			s.vmSize = firstFieldUint(val)
		case "VmSwap":
			s.vmSwap = firstFieldUint(val)
		case "Threads":
			s.threads = int32(firstFieldUint(val))
		case "voluntary_ctxt_switches":
			s.volctx = firstFieldUint(val)
		case "nonvoluntary_ctxt_switches":
			s.nonvolctx = firstFieldUint(val)
		}
	}
	return s
}

// parseCmdline splits the NUL-separated /proc/<pid>/cmdline into arguments,
// dropping empties (a kernel thread has an empty cmdline).
func parseCmdline(b []byte) []string {
	var out []string
	for _, a := range strings.Split(string(b), "\x00") {
		if a != "" {
			out = append(out, a)
		}
	}
	return out
}

// parseIO reads the cumulative counters from /proc/<pid>/io.
func parseIO(data string, p *Proc) {
	for _, line := range strings.Split(data, "\n") {
		key, val, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		n := firstFieldUint(strings.TrimSpace(val))
		switch key {
		case "syscr":
			p.ReadCount = n
		case "syscw":
			p.WriteCount = n
		case "read_bytes":
			p.ReadBytes = n
		case "write_bytes":
			p.WriteBytes = n
		}
	}
}

// linuxState maps a /proc state character to the wire enum.
func linuxState(c byte) ProcState {
	switch c {
	case 'R':
		return stateR
	case 'S', 'I': // sleeping, or idle kernel thread
		return stateS
	case 'D':
		return stateD
	case 'T', 't':
		return stateT
	case 'Z':
		return stateZ
	case 'X', 'x':
		return stateX
	case 'W':
		return stateW
	default:
		return stateU
	}
}

// hostTotalMemory returns total physical RAM in bytes from /proc/meminfo's
// MemTotal (kB), matching the host package's system.mem.total. 0 if unreadable.
func hostTotalMemory() int64 {
	data, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(data), "\n") {
		if rest, ok := strings.CutPrefix(line, "MemTotal:"); ok {
			return int64(firstFieldUint(strings.TrimSpace(rest))) * 1024 // kB -> bytes
		}
	}
	return 0
}

func readBtime(path string) uint64 {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(data), "\n") {
		if rest, ok := strings.CutPrefix(line, "btime "); ok {
			return atou(strings.TrimSpace(rest))
		}
	}
	return 0
}

func firstFieldUint(s string) uint64 {
	if f := strings.Fields(s); len(f) > 0 {
		return atou(f[0])
	}
	return 0
}

func firstFieldInt32(s string) int32 {
	if f := strings.Fields(s); len(f) > 0 {
		return atoi32(f[0])
	}
	return 0
}
