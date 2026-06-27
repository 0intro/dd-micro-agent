//go:build openbsd || netbsd

package host

// Disk I/O (system.io.*) on OpenBSD and NetBSD. x/sys exposes no helper for the HW_DISKSTATS
// / HW_IOSTATS nodes, so the MIB is built numerically and read through the raw __sysctl
// syscall (the same technique internal/process uses for KERN_PROC), then decoded by the
// neutral parser. The per-OS files supply the syscall trap, the MIB, and the parser.

import (
	"time"
	"unsafe"

	"golang.org/x/sys/unix"

	"github.com/0intro/dd-micro-agent/internal/metrics"
)

// bsdIO diffs the kernel's per-device cumulative counters between reads into rates.
type bsdIO struct {
	prev   map[string]devstatRec
	prevTs time.Time
}

func (*bsdIO) name() string { return "diskio" }

func (c *bsdIO) collect(now time.Time) ([]metrics.Serie, error) {
	b, err := diskIOBlob()
	if err != nil {
		return nil, err
	}
	cur := make(map[string]devstatRec)
	for _, r := range parseDiskIO(b) {
		cur[r.name] = r
	}

	var out []metrics.Serie
	if c.prev != nil {
		if dt := now.Sub(c.prevTs).Seconds(); dt > 0 {
			for name, r := range cur {
				if p, ok := c.prev[name]; ok {
					out = append(out, bsdIOSeries(name, r, p, dt, now)...)
				}
			}
		}
	}
	c.prev, c.prevTs = cur, now
	return out, nil
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

// diskIOBlobMIB reads a whole HW_DISKSTATS / HW_IOSTATS array through a size-then-fetch,
// growing the buffer if a disk is attached between the two calls. The per-OS diskIOBlob
// supplies the MIB.
func diskIOBlobMIB(mib []int32) ([]byte, error) {
	size, err := sysctlSize(mib)
	if err != nil || size == 0 {
		return nil, err
	}
	buf := make([]byte, size+2*ioStride) // slop for a disk attached since the size call
	n, err := sysctlInto(mib, buf)
	if err != nil {
		return nil, err
	}
	return buf[:n], nil
}
