package host

import "golang.org/x/sys/unix"

// NetBSD disk I/O reads the HW_IOSTATS node. Its MIB carries the record size as a third
// element so the kernel can version the struct io_sysctl it returns.
const (
	sysctlTrap = unix.SYS___SYSCTL
	ctlHW      = 6
	hwIostats  = 9
	ioStride   = nbIoStride
)

func diskIOBlob() ([]byte, error) { return diskIOBlobMIB([]int32{ctlHW, hwIostats, nbIoStride}) }

func parseDiskIO(b []byte) []devstatRec { return parseNetBSDIostats(b) }
