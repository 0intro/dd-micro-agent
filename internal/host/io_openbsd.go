package host

import "golang.org/x/sys/unix"

// OpenBSD disk I/O reads the HW_DISKSTATS node (a two-element MIB, an array of struct
// diskstats sized by hw.diskcount).
const (
	sysctlTrap  = unix.SYS_SYSCTL
	ctlHW       = 6
	hwDiskstats = 9
	ioStride    = obDsStride
)

func diskIOBlob() ([]byte, error) { return diskIOBlobMIB([]int32{ctlHW, hwDiskstats}) }

func parseDiskIO(b []byte) []devstatRec { return parseOpenBSDDiskstats(b) }
