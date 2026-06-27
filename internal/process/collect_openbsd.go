//go:build openbsd

package process

import "golang.org/x/sys/unix"

// OpenBSD specifics for the shared BSD2 collector: the __sysctl trap, the KERN_PROC
// op, the struct kinfo_proc stride, and the parsers.
const (
	sysctlTrap   = unix.SYS_SYSCTL
	kernProcOp   = 66 // KERN_PROC
	kernProcArgs = 55 // KERN_PROC_ARGS
	procStride   = obStride
)

func parseProcsBlob(b []byte) []Proc { return parseOpenBSDProcs(b) }

// decodeArgv unpacks OpenBSD's KERN_PROC_ARGV buffer, which leads with an argv
// pointer vector (see parseOpenBSDArgv).
func decodeArgv(b []byte) []string { return parseOpenBSDArgv(b) }

// hostTotalMemory returns total RAM in bytes. OpenBSD's sysctl name table has no
// hw.physmem64, so derive it from the UVM page accounting, matching host.openbsdMem.
func hostTotalMemory() int64 {
	u, err := unix.SysctlUvmexp("vm.uvmexp")
	if err != nil {
		return 0
	}
	return int64(u.Npages) * int64(u.Pagesize)
}
