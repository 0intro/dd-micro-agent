//go:build openbsd

package hostmeta

import "golang.org/x/sys/unix"

// unixFilesystem lists mounted filesystems via getfsstat. OpenBSD's struct statfs
// uses F_-prefixed field names and counts blocks in F_bsize units.
func unixFilesystem() []gohaiFS {
	n, err := unix.Getfsstat(nil, unix.MNT_NOWAIT)
	if err != nil || n == 0 {
		return []gohaiFS{}
	}
	buf := make([]unix.Statfs_t, n)
	if _, err := unix.Getfsstat(buf, unix.MNT_NOWAIT); err != nil {
		return []gohaiFS{}
	}
	out := []gohaiFS{}
	for i := range buf {
		st := &buf[i]
		if st.F_blocks == 0 {
			continue
		}
		out = append(out, gohaiFS{
			Name:      unix.ByteSliceToString(st.F_mntfromname[:]),
			SizeKB:    st.F_blocks * uint64(st.F_bsize) / 1024,
			MountedOn: unix.ByteSliceToString(st.F_mntonname[:]),
		})
	}
	return out
}
