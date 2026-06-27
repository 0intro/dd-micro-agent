//go:build darwin || freebsd || dragonfly

package hostmeta

import "golang.org/x/sys/unix"

// unixFilesystem lists mounted filesystems via getfsstat. darwin and freebsd
// share the Statfs_t field names (Bsize/Blocks/Mntonname). openbsd and netbsd
// differ and have their own readers in gohai_fs_openbsd.go and gohai_fs_netbsd.go.
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
		if st.Blocks == 0 {
			continue
		}
		out = append(out, gohaiFS{
			Name:      unix.ByteSliceToString(st.Mntfromname[:]),
			SizeKB:    uint64(st.Blocks) * uint64(st.Bsize) / 1024,
			MountedOn: unix.ByteSliceToString(st.Mntonname[:]),
		})
	}
	return out
}
