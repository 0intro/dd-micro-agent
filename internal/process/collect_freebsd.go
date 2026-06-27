//go:build freebsd

package process

// FreeBSD process collection via sysctl. kern.proc.all returns the whole process table
// as an array of struct kinfo_proc (decoded by the neutral parseKinfoProcs). A per-PID
// kern.proc.args sysctl gives the argument vector. No cgo and no /proc: x/sys makes the
// sysctl call, encoding/binary does the unpacking. The genuine kinfo_proc gaps (cwd,
// open-fd count, per-process swap, IO byte counters) stay zero, which the intake
// tolerates. Threads, RSS/VMS, CPU times, and context switches are all present.

import (
	"errors"

	"golang.org/x/sys/unix"
)

func collectProcs() ([]Proc, error) {
	b, err := sysctlProcAll()
	if err != nil {
		return nil, err
	}
	procs := parseKinfoProcs(b)
	users := map[int32]string{} // uid -> name, resolved once per collection
	for i := range procs {
		p := &procs[i]
		p.User = lookupUser(p.Uid, users)
		// Argument vector (best-effort: a kernel process has none, and an unprivileged
		// agent may be denied another user's args by security.bsd.unprivileged_proc_debug).
		if args, err := unix.SysctlRaw("kern.proc.args", int(p.Pid)); err == nil {
			p.Args = parseArgv(args)
		}
		if len(p.Args) > 0 {
			p.Exe = p.Args[0]
		} else {
			p.Args, p.Exe = []string{p.Name}, p.Name
		}
	}
	return procs, nil
}

// sysctlProcAll reads the kern.proc.all blob, retrying the transient ENOMEM the
// size-then-fetch race can hit under process churn: SysctlRaw sizes the buffer in one
// call and fills it in a second, so a fork in between makes the table outgrow the
// buffer and the fill fails. Each retry re-sizes from scratch and catches up. ps and
// libkvm loop the same way.
func sysctlProcAll() ([]byte, error) {
	var err error
	for try := 0; try < 4; try++ {
		var b []byte
		if b, err = unix.SysctlRaw("kern.proc.all"); err == nil {
			return b, nil
		}
		if !errors.Is(err, unix.ENOMEM) {
			return nil, err
		}
	}
	return nil, err
}

// hostTotalMemory returns total physical RAM in bytes from sysctl hw.physmem, matching
// the host package's system.mem.total (which reads the same MIB). 0 if unreadable.
func hostTotalMemory() int64 {
	n, err := unix.SysctlUint64("hw.physmem")
	if err != nil {
		return 0
	}
	return int64(n)
}
