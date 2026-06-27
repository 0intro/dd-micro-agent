//go:build netbsd

package process

import "golang.org/x/sys/unix"

// NetBSD specifics for the shared BSD2 collector: the __sysctl trap, the KERN_PROC2
// op, the struct kinfo_proc2 stride, and the parsers.
const (
	sysctlTrap   = unix.SYS___SYSCTL
	kernProcOp   = 47 // KERN_PROC2
	kernProcArgs = 48 // KERN_PROC_ARGS (55 here is KERN_MAXPTYS)
	procStride   = nbStride
)

func parseProcsBlob(b []byte) []Proc { return parseNetBSDProcs(b) }

// decodeArgv unpacks NetBSD's KERN_PROC_ARGV buffer, plain NUL-separated strings.
func decodeArgv(b []byte) []string { return parseArgv(b) }

// hostTotalMemory returns total RAM in bytes. NetBSD resolves sysctl names at
// runtime, so hw.physmem64 is available.
func hostTotalMemory() int64 {
	n, err := unix.SysctlUint64("hw.physmem64")
	if err != nil {
		return 0
	}
	return int64(n)
}
