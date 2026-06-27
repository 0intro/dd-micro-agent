package host

import (
	"bufio"
	"bytes"
	"errors"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/0intro/dd-micro-agent/internal/metrics"
)

// fsCollector reports disk-space and inode usage per real mounted filesystem,
// reading the mount table from /proc/mounts and the figures from statfs. Values
// are reported in kB (legacy unit of the stock disk check). statfs is a field so
// tests can inject a fake.
type fsCollector struct {
	c      *Collector
	statfs func(path string, buf *syscall.Statfs_t) error
}

func statfs(path string, buf *syscall.Statfs_t) error { return syscall.Statfs(path, buf) }

const statfsTimeout = 5 * time.Second

// statfsCall runs one statfs under a deadline. A hung network mount (a hard
// NFS server gone away) blocks statfs in uninterruptible sleep, and without
// the bound it would freeze the aggregator's flush goroutine with it. On
// timeout the goroutine is abandoned, as the stock Agent's wrapper abandons
// its call.
func (fc *fsCollector) statfsCall(mount string) (syscall.Statfs_t, error) {
	type result struct {
		st  syscall.Statfs_t
		err error
	}
	ch := make(chan result, 1)
	go func() {
		var r result
		r.err = fc.statfs(mount, &r.st)
		ch <- r
	}()
	select {
	case r := <-ch:
		return r.st, r.err
	case <-time.After(statfsTimeout):
		return syscall.Statfs_t{}, errors.New("statfs timed out")
	}
}

// pseudoFS are filesystems with no real disk usage worth reporting: kernel
// virtual filesystems, plus read-only image mounts (squashfs/iso9660) such as
// the loopback devices snap creates, which would otherwise show up as a swarm
// of 100%-full disks.
var pseudoFS = map[string]bool{
	"autofs": true, "binfmt_misc": true, "bpf": true, "cgroup": true,
	"cgroup2": true, "configfs": true, "debugfs": true, "devpts": true,
	"devtmpfs": true, "fusectl": true, "hugetlbfs": true, "iso9660": true,
	"mqueue": true, "nsfs": true, "overlay": true, "proc": true,
	"pstore": true, "ramfs": true, "securityfs": true, "squashfs": true,
	"sysfs": true, "tmpfs": true, "tracefs": true,
}

func (fc *fsCollector) name() string { return "filesystem" }

func (fc *fsCollector) collect(now time.Time) ([]metrics.Serie, error) {
	data, err := fc.c.readProc("mounts")
	if err != nil {
		return nil, err
	}

	var out []metrics.Serie
	seen := make(map[string]bool)
	sc := bufio.NewScanner(bytes.NewReader(data))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) < 3 {
			continue
		}
		dev, mount, fstype := unescape(f[0]), unescape(f[1]), f[2]
		if pseudoFS[fstype] || seen[dev] {
			continue
		}

		st, err := fc.statfsCall(mount)
		if err != nil {
			continue
		}
		if st.Blocks == 0 {
			continue
		}
		seen[dev] = true

		bsize := uint64(st.Bsize)
		const kb = 1024.0
		total := float64(st.Blocks*bsize) / kb
		free := float64(st.Bavail*bsize) / kb
		used := float64((st.Blocks-st.Bfree)*bsize) / kb

		tags := []string{"device:" + dev, "device_name:" + filepath.Base(dev)}
		out = append(out,
			gauge("system.disk.total", now, total, tags...),
			gauge("system.disk.used", now, used, tags...),
			gauge("system.disk.free", now, free, tags...),
			// used/(used+free) is the stock Agent's UsedPercent: the root
			// reserve is excluded, so a filesystem full for non-root reads 1.0.
			gauge("system.disk.in_use", now, ratio(used, used+free), tags...),
		)

		if st.Files > 0 {
			it := float64(st.Files)
			iused := float64(st.Files - st.Ffree)
			out = append(out,
				gauge("system.fs.inodes.total", now, it, tags...),
				gauge("system.fs.inodes.used", now, iused, tags...),
				gauge("system.fs.inodes.free", now, float64(st.Ffree), tags...),
				gauge("system.fs.inodes.in_use", now, ratio(iused, it), tags...),
			)
		}
	}
	return out, nil
}

// unescape resolves the octal escapes (\040 space, \011 tab, \012 newline,
// \134 backslash) that /proc/mounts uses for those characters in a path.
func unescape(s string) string {
	if !strings.ContainsRune(s, '\\') {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+3 < len(s) {
			if c, ok := octal(s[i+1], s[i+2], s[i+3]); ok {
				b.WriteByte(c)
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

func octal(a, b, c byte) (byte, bool) {
	if a < '0' || a > '7' || b < '0' || b > '7' || c < '0' || c > '7' {
		return 0, false
	}
	return (a-'0')<<6 | (b-'0')<<3 | (c - '0'), true
}
