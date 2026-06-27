package hostmeta

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func TestGohaiPlatform(t *testing.T) {
	dir := procDir(t, map[string]string{
		"sys/kernel/ostype":    "Linux\n",
		"sys/kernel/osrelease": "6.8.0-110-generic\n",
		"sys/kernel/version":   "#110-Ubuntu SMP PREEMPT_DYNAMIC\n",
	})
	p := (&gohaiCollector{proc: dir}).platform("myhost")
	if p.KernelName != "Linux" || p.KernelRelease != "6.8.0-110-generic" {
		t.Errorf("kernel = %q / %q", p.KernelName, p.KernelRelease)
	}
	if p.Hostname != "myhost" || p.OS != "GNU/Linux" {
		t.Errorf("platform = %+v", p)
	}
	if p.GoOS == "" || p.Machine == "" {
		t.Errorf("runtime fields not set: %+v", p)
	}
}

func TestGohaiCPU(t *testing.T) {
	dir := procDir(t, map[string]string{"cpuinfo": cpuinfo})
	c := (&gohaiCollector{proc: dir}).cpu()
	if c.VendorID != "GenuineIntel" || c.ModelName != "Test CPU @ 2.40GHz" {
		t.Errorf("cpu identity = %+v", c)
	}
	if c.CPULogicalProcessors != 2 {
		t.Errorf("logical = %d, want 2", c.CPULogicalProcessors)
	}
	if c.CPUCores != 2 { // two distinct core ids on one physical package
		t.Errorf("cores = %d, want 2", c.CPUCores)
	}
	if c.Mhz != 2400 {
		t.Errorf("mhz = %v, want 2400", c.Mhz)
	}
}

func TestGohaiMemory(t *testing.T) {
	dir := procDir(t, map[string]string{"meminfo": "MemTotal:        2048 kB\nSwapTotal:       4096 kB\n"})
	m := (&gohaiCollector{proc: dir}).memory()
	if m.Total != 2048*1024 { // bytes
		t.Errorf("total = %d, want %d", m.Total, 2048*1024)
	}
	if m.SwapTotal != 4096 { // kB
		t.Errorf("swap_total = %d, want 4096", m.SwapTotal)
	}
}

func TestGohaiFilesystem(t *testing.T) {
	dir := procDir(t, map[string]string{"mounts": `/dev/sda1 / ext4 rw 0 0
proc /proc proc rw 0 0
tmpfs /run tmpfs rw 0 0
`})
	c := &gohaiCollector{
		proc: dir,
		statfs: func(path string, buf *syscall.Statfs_t) error {
			if path != "/" {
				t.Errorf("unexpected statfs path %q (pseudo-fs should be skipped)", path)
			}
			*buf = syscall.Statfs_t{Bsize: 4096, Blocks: 1000}
			return nil
		},
	}
	fs := c.filesystem()
	if len(fs) != 1 {
		t.Fatalf("got %d filesystems, want 1: %+v", len(fs), fs)
	}
	if fs[0].Name != "/dev/sda1" || fs[0].MountedOn != "/" || fs[0].SizeKB != 4000 {
		t.Errorf("fs = %+v, want /dev/sda1 / 4000kB", fs[0])
	}
}

func TestGohaiFilesystemEscapes(t *testing.T) {
	dir := procDir(t, map[string]string{"mounts": `/dev/disk/by-label/My\040Disk /mnt/backup\040drive ext4 rw 0 0
`})
	c := &gohaiCollector{
		proc: dir,
		statfs: func(path string, buf *syscall.Statfs_t) error {
			if path != "/mnt/backup drive" {
				t.Errorf("statfs path = %q, want the octal escape decoded", path)
			}
			*buf = syscall.Statfs_t{Bsize: 4096, Blocks: 1000}
			return nil
		},
	}
	fs := c.filesystem()
	if len(fs) != 1 {
		t.Fatalf("got %d filesystems, want 1: %+v", len(fs), fs)
	}
	if fs[0].Name != "/dev/disk/by-label/My Disk" || fs[0].MountedOn != "/mnt/backup drive" {
		t.Errorf("fs = %+v, want the \\040 escapes decoded in device and mount", fs[0])
	}
}

func TestUnescape(t *testing.T) {
	for in, want := range map[string]string{
		"/plain":           "/plain",
		`/mnt/a\040b`:      "/mnt/a b",
		`/tab\011sep`:      "/tab\tsep",
		`/back\134slash`:   `/back\slash`,
		`/not\04octal`:     `/not\04octal`,
		`/trailing\0`:      `/trailing\0`,
		`/two\040\040gaps`: "/two  gaps",
	} {
		if got := unescape(in); got != want {
			t.Errorf("unescape(%q) = %q, want %q", in, got, want)
		}
	}
}

const cpuinfo = `processor	: 0
vendor_id	: GenuineIntel
cpu family	: 6
model		: 158
model name	: Test CPU @ 2.40GHz
stepping	: 10
cpu MHz		: 2400.000
physical id	: 0
core id		: 0

processor	: 1
vendor_id	: GenuineIntel
cpu family	: 6
model		: 158
model name	: Test CPU @ 2.40GHz
stepping	: 10
cpu MHz		: 2400.000
physical id	: 0
core id		: 1
`

func procDir(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, content := range files {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}
