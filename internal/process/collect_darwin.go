//go:build darwin

package process

// macOS process collection. kinfo_proc (sysctl) can't give thread counts or
// reliable RSS without libproc (cgo), and the backend drops zero-thread processes.
// So, like gopsutil, we shell out to ps, one of the agent's two execs (the other
// is journalctl, see internal/logs). The output parsing lives in darwinps.go
// (pure, unit-tested on the dev host).

import (
	"context"
	"os/exec"
	"time"

	"golang.org/x/sys/unix"
)

func collectProcs() ([]Proc, error) {
	// The deadline bounds a wedged ps (a hung directory service can block it),
	// so one bad collect cannot hang the Reporter and shutdown behind it.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Identity + resources. command is last because it contains spaces. Every other
	// column is a single token. -ww disables column truncation of the command.
	out, err := exec.CommandContext(ctx, "ps", "-axww", "-o",
		"pid,ppid,uid,gid,rss,vsz,state,time,etime,user,command").Output()
	if err != nil {
		return nil, err
	}
	procs := parsePsProcs(string(out), time.Now().UnixMilli())

	// Real per-process thread counts, best-effort: ps -M lists threads under each
	// process. If it fails or doesn't resolve a PID, that proc keeps Threads=1.
	if tout, err := exec.CommandContext(ctx, "ps", "-axM").Output(); err == nil {
		threads := parsePsThreads(string(tout))
		for i := range procs {
			if n := threads[procs[i].Pid]; n > 0 {
				procs[i].Threads = int32(n)
			}
		}
	}
	return procs, nil
}

// hostTotalMemory returns total physical RAM in bytes (hw.memsize), 0 if the
// sysctl fails.
func hostTotalMemory() int64 {
	n, err := unix.SysctlUint64("hw.memsize")
	if err != nil {
		return 0
	}
	return int64(n)
}
