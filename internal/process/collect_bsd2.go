//go:build openbsd || netbsd

package process

// Process collection on OpenBSD and NetBSD. x/sys exposes no kinfo_proc(2) helper,
// and OpenBSD's sysctl name table even lacks kern.proc_args, so the MIB is built
// numerically and read through the raw __sysctl syscall, then decoded by the neutral
// parser (parseOpenBSDProcs / parseNetBSDProcs). The per-OS collect files supply the
// syscall trap, the KERN_PROC op, the record stride, and the parser. Fields not in
// the struct (cwd, open-fd count, swap, IO byte counters) stay zero, which the
// intake tolerates.

import (
	"errors"
	"unsafe"

	"golang.org/x/sys/unix"
)

// ctlKern and the sub-op values shared by both kernels. kernProcArgs differs
// per OS and lives beside kernProcOp in the per-OS files.
const (
	ctlKern      = 1
	kernProcAll  = 0
	kernProcArgv = 1 // KERN_PROC_ARGV
)

func collectProcs() ([]Proc, error) {
	b, err := sysctlProcList()
	if err != nil {
		return nil, err
	}
	procs := parseProcsBlob(b)
	users := map[int32]string{} // uid -> name, resolved once per collection
	for i := range procs {
		p := &procs[i]
		p.User = lookupUser(p.Uid, users)
		// Argument vector, best-effort: a kernel thread has none, and a process owned
		// by another user may be denied.
		if args := procArgs(p.Pid); len(args) > 0 {
			p.Args = args
			p.Exe = args[0]
		} else {
			p.Args, p.Exe = []string{p.Name}, p.Name
		}
	}
	return procs, nil
}

// RawProcBlob returns the raw process sysctl blob plus the record stride. The e2e
// capture tool uses it to verify the parser offsets against a live kernel.
func RawProcBlob() ([]byte, int, error) {
	b, err := sysctlProcList()
	return b, procStride, err
}

// sysctlProcList reads the whole process table, retrying the transient ENOMEM the
// size-then-fetch race hits under process churn (a fork between the two calls makes
// the table outgrow the buffer). ps and libkvm loop the same way.
func sysctlProcList() ([]byte, error) {
	var err error
	for try := 0; try < 4; try++ {
		var b []byte
		if b, err = procListOnce(); err == nil {
			return b, nil
		}
		if !errors.Is(err, unix.ENOMEM) {
			return nil, err
		}
	}
	return nil, err
}

func procListOnce() ([]byte, error) {
	mib := []int32{ctlKern, kernProcOp, kernProcAll, 0, int32(procStride), 0}
	size, err := sysctlSize(mib)
	if err != nil {
		return nil, err
	}
	if size == 0 {
		return nil, nil
	}
	size += size/8 + 16*procStride // slop for processes forked since the size call
	buf := make([]byte, size)
	mib[5] = int32(len(buf) / procStride) // element count for the fetch
	n, err := sysctlInto(mib, buf)
	if err != nil {
		return nil, err
	}
	return buf[:n], nil
}

func procArgs(pid int32) []string {
	mib := []int32{ctlKern, kernProcArgs, pid, kernProcArgv}
	size, err := sysctlSize(mib)
	if err != nil || size == 0 {
		return nil
	}
	buf := make([]byte, size)
	n, err := sysctlInto(mib, buf)
	if err != nil {
		return nil
	}
	return decodeArgv(buf[:n])
}

// sysctlSize asks the kernel for a MIB's byte size (oldp == NULL).
func sysctlSize(mib []int32) (int, error) {
	var n uintptr
	if _, _, e := unix.Syscall6(uintptr(sysctlTrap),
		uintptr(unsafe.Pointer(&mib[0])), uintptr(len(mib)),
		0, uintptr(unsafe.Pointer(&n)), 0, 0); e != 0 {
		return 0, e
	}
	return int(n), nil
}

// sysctlInto fills buf from a MIB and returns the byte count written.
func sysctlInto(mib []int32, buf []byte) (int, error) {
	n := uintptr(len(buf))
	if _, _, e := unix.Syscall6(uintptr(sysctlTrap),
		uintptr(unsafe.Pointer(&mib[0])), uintptr(len(mib)),
		uintptr(unsafe.Pointer(&buf[0])), uintptr(unsafe.Pointer(&n)), 0, 0); e != 0 {
		return 0, e
	}
	return int(n), nil
}
