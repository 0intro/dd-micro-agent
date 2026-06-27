//go:build plan9

package process

// Plan 9 process collection from /proc: plain text file reads, no syscalls.
// status gives name, user, state, CPU times, the process's wall-clock age (→ create
// time) and memory. args gives the command line. fd gives the working directory and
// the open-fd count. segment gives the shared-memory size from its per-segment
// reference counts. A Plan 9 proc is a single thread of execution, so threads is 1.
// The genuine platform gaps (devproc.c exposes no such file) are ppid, numeric
// uid/gid, per-process IO, and context switches. Those fields stay zero.

import (
	"os"
	"time"
)

func collectProcs() ([]Proc, error) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, err
	}
	nowMs := time.Now().UnixMilli()
	out := make([]Proc, 0, len(entries))
	for _, e := range entries {
		pid, ok := parsePid(e.Name())
		if !ok {
			continue
		}
		base := "/proc/" + e.Name()
		data, err := os.ReadFile(base + "/status")
		if err != nil {
			continue // exited between ReadDir and now
		}
		st, ok := parsePlan9Status(string(data))
		if !ok {
			continue
		}
		p := Proc{
			Pid: pid, NsPid: pid,
			Name:       st.name,
			Exe:        st.name,
			User:       st.user,
			State:      plan9State(st.state),
			Threads:    1, // a Plan 9 proc is a single thread of execution
			UserTime:   float64(st.utimeMs) / 1000,
			SystemTime: float64(st.stimeMs) / 1000,
			// Plan 9's status memory is the summed segment size (virtual address
			// space, not a resident/virtual split), so report it as both Vms and
			// (approximately) Rss. On Plan 9 the two are essentially the same.
			Rss: st.memKB * 1024,
			Vms: st.memKB * 1024,
		}
		// TReal is the process's age in ms. createTime = now - age. Guard against a
		// nonsensical age (clock skew / a just-forked proc) so we never post a future
		// timestamp. 0 then means "unknown", which the intake tolerates.
		if st.realMs > 0 && int64(st.realMs) <= nowMs {
			p.CreateTime = nowMs - int64(st.realMs)
		}
		if args, err := os.ReadFile(base + "/args"); err == nil {
			p.Args = parsePlan9Args(string(args))
		}
		if len(p.Args) == 0 {
			p.Args = []string{st.name}
		}
		// /proc/<pid>/fd is the cwd (line 1) then one line per open fd, best-effort.
		if fds, err := os.ReadFile(base + "/fd"); err == nil {
			cwd, n := parsePlan9Fds(string(fds))
			p.Cwd, p.OpenFd = cwd, int32(n)
		}
		// /proc/<pid>/segment carries a reference count per segment. Those mapped
		// into more than one process are shared memory, best-effort.
		if seg, err := os.ReadFile(base + "/segment"); err == nil {
			p.Shared = parsePlan9Shared(string(seg))
		}
		out = append(out, p)
	}
	return out, nil
}

// hostTotalMemory returns total physical RAM in bytes from /dev/swap's "memory"
// line (the only kernel source for it: /env/swap holds swap capacity, not RAM).
// 0 if unreadable.
func hostTotalMemory() int64 {
	data, err := os.ReadFile("/dev/swap")
	if err != nil {
		return 0
	}
	return int64(parsePlan9MemTotal(string(data)))
}
