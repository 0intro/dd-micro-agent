//go:build netbsd

package hostmeta

import "golang.org/x/sys/unix"

// unixFilesystem lists mounted filesystems via getvfsstat. NetBSD's struct statvfs
// counts blocks in Frsize (fragment) units.
func unixFilesystem() []gohaiFS {
	n, err := unix.Getvfsstat(nil, unix.ST_NOWAIT)
	if err != nil || n == 0 {
		return []gohaiFS{}
	}
	buf := make([]unix.Statvfs_t, n)
	if _, err := unix.Getvfsstat(buf, unix.ST_NOWAIT); err != nil {
		return []gohaiFS{}
	}
	out := []gohaiFS{}
	for i := range buf {
		st := &buf[i]
		unit := st.Frsize
		if unit == 0 {
			unit = st.Bsize
		}
		if st.Blocks == 0 {
			continue
		}
		out = append(out, gohaiFS{
			Name:      unix.ByteSliceToString(st.Mntfromname[:]),
			SizeKB:    st.Blocks * unit / 1024,
			MountedOn: unix.ByteSliceToString(st.Mntonname[:]),
		})
	}
	return out
}
