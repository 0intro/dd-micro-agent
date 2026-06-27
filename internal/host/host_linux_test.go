package host

import (
	"math"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
	"time"

	"github.com/0intro/dd-micro-agent/internal/metrics"
)

var t0 = time.Unix(1_700_000_000, 0)

// TestCollectAgainstRealProc exercises every collector against the host's real
// /proc, catching parsing assumptions that fixtures might not. It collects twice
// so the rate collectors produce output.
func TestCollectAgainstRealProc(t *testing.T) {
	if _, err := os.Stat("/proc/meminfo"); err != nil {
		t.Skip("no /proc")
	}
	c := New(Options{})
	c.Collect()                       // baseline for the rate collectors
	time.Sleep(25 * time.Millisecond) // two jiffies, so the cpu counters have moved
	got := values(c.Collect())

	for _, name := range []string{"system.mem.total", "system.uptime", "system.cpu.num_cores"} {
		if got[name] <= 0 {
			t.Errorf("%s = %v, want > 0 from real /proc", name, got[name])
		}
	}
	if _, ok := got["system.cpu.user"]; !ok {
		t.Error("expected system.cpu.user on the second collect")
	}
}

func TestMemory(t *testing.T) {
	dir := procDir(t, map[string]string{"meminfo": `MemTotal:       2048 kB
MemFree:         512 kB
MemAvailable:   1024 kB
Cached:          256 kB
Buffers:         128 kB
Shmem:            64 kB
Slab:             32 kB
SwapTotal:      4096 kB
SwapFree:       2048 kB
`})
	series, err := (&memCollector{c: &Collector{proc: dir}}).collect(t0)
	if err != nil {
		t.Fatal(err)
	}
	assertValues(t, values(series), map[string]float64{
		"system.mem.total":      2,
		"system.mem.free":       0.5,
		"system.mem.used":       1.5,
		"system.mem.usable":     1,
		"system.mem.pct_usable": 0.5,
		"system.mem.cached":     0.25,
		"system.swap.total":     4,
		"system.swap.used":      2,
	})
}

func TestCPUPercentages(t *testing.T) {
	dir := t.TempDir()
	cc := &cpuCollector{c: &Collector{proc: dir}}

	// First read is an all-zero baseline: only num_cores, no percentages yet.
	writeProc(t, dir, "stat", "cpu  0 0 0 0 0 0 0 0 0 0\ncpu0 0 0 0 0 0 0 0\ncpu1 0 0 0 0 0 0 0 0\n")
	base, err := cc.collect(t0)
	if err != nil {
		t.Fatal(err)
	}
	bv := values(base)
	if bv["system.cpu.num_cores"] != 2 {
		t.Errorf("num_cores = %v, want 2", bv["system.cpu.num_cores"])
	}
	if _, ok := bv["system.cpu.user"]; ok {
		t.Error("first read should not emit percentages")
	}

	// Second read: total delta = 1000 jiffies, chosen so percentages are exact.
	writeProc(t, dir, "stat", "cpu  100 0 50 800 20 0 10 20 0 0\ncpu0 0 0 0 0 0 0 0 0\ncpu1 0 0 0 0 0 0 0 0\n")
	got, err := cc.collect(t0.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	assertValues(t, values(got), map[string]float64{
		"system.cpu.user":   10, // 100/1000
		"system.cpu.system": 6,  // (50+10)/1000
		"system.cpu.iowait": 2,  // 20/1000
		"system.cpu.idle":   80, // 800/1000
		"system.cpu.stolen": 2,  // 20/1000
	})
}

func TestLoad(t *testing.T) {
	dir := procDir(t, map[string]string{"loadavg": "2.0 4.0 8.0 1/100 1234\n"})
	series, err := (&loadCollector{c: &Collector{proc: dir}, ncpu: 4}).collect(t0)
	if err != nil {
		t.Fatal(err)
	}
	assertValues(t, values(series), map[string]float64{
		"system.load.1":      2,
		"system.load.5":      4,
		"system.load.15":     8,
		"system.load.norm.1": 0.5,
		"system.load.norm.5": 1,
	})
}

func TestUptime(t *testing.T) {
	dir := procDir(t, map[string]string{"uptime": "12345.67 98765.43\n"})
	series, err := (&uptimeCollector{c: &Collector{proc: dir}}).collect(t0)
	if err != nil {
		t.Fatal(err)
	}
	assertValues(t, values(series), map[string]float64{"system.uptime": 12345.67})
}

func TestFilesystem(t *testing.T) {
	dir := procDir(t, map[string]string{"mounts": `/dev/sda1 / ext4 rw 0 0
proc /proc proc rw 0 0
tmpfs /run tmpfs rw 0 0
/dev/loop0 /snap/core/1 squashfs ro 0 0
/dev/sda1 /mnt/dup ext4 rw 0 0
`})
	fc := &fsCollector{
		c: &Collector{proc: dir},
		statfs: func(path string, buf *syscall.Statfs_t) error {
			if path != "/" {
				t.Errorf("unexpected statfs path %q (pseudo-fs and dup device should be skipped)", path)
				return syscall.ENOENT
			}
			*buf = syscall.Statfs_t{Bsize: 4096, Blocks: 1000, Bfree: 400, Bavail: 300, Files: 100, Ffree: 60}
			return nil
		},
	}
	series, err := fc.collect(t0)
	if err != nil {
		t.Fatal(err)
	}
	assertValues(t, values(series), map[string]float64{
		"system.disk.total":       4000,      // 1000*4096/1024
		"system.disk.used":        2400,      // 600*4096/1024
		"system.disk.free":        1200,      // 300*4096/1024
		"system.disk.in_use":      2.0 / 3.0, // used/(used+free): the root reserve (Bfree-Bavail) is excluded
		"system.fs.inodes.used":   40,
		"system.fs.inodes.in_use": 0.4,
	})
	for _, s := range series {
		assertHasTags(t, s, "device:/dev/sda1", "device_name:sda1")
	}
}

func TestUnescape(t *testing.T) {
	tests := []struct{ in, want string }{
		{"/media/My\\040Disk", "/media/My Disk"},
		{"a\\134b", `a\b`},
		{"/plain/path", "/plain/path"},
		{"trailing\\04", "trailing\\04"}, // truncated escape stays literal
		{"not\\999octal", "not\\999octal"},
	}
	for _, tt := range tests {
		if got := unescape(tt.in); got != tt.want {
			t.Errorf("unescape(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestNetworkRates(t *testing.T) {
	dir := t.TempDir()
	nc := &netCollector{c: &Collector{proc: dir}}

	writeProc(t, dir, "net/dev", netDev(1000, 10, 2000, 20))
	first, err := nc.collect(t0)
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 0 {
		t.Errorf("first network read emitted %d series, want 0", len(first))
	}

	writeProc(t, dir, "net/dev", netDev(2000, 30, 5000, 50))
	series, err := nc.collect(t0.Add(10 * time.Second))
	if err != nil {
		t.Fatal(err)
	}
	assertValues(t, values(series), map[string]float64{
		"system.net.bytes_rcvd":        100, // (2000-1000)/10
		"system.net.bytes_sent":        300, // (5000-2000)/10
		"system.net.packets_in.count":  2,   // (30-10)/10
		"system.net.packets_out.count": 3,   // (50-20)/10
	})
}

func TestDiskIORates(t *testing.T) {
	dir := t.TempDir()
	ic := &ioCollector{c: &Collector{proc: dir}}

	// /proc/diskstats: major minor name reads rmerged rsect msread writes wmerged
	// wsect mswrite inflight msio wmsio. loop0 must be excluded.
	const first = "   8 0 vda 1000 0 4000 100 2000 0 8000 200 0 500 700\n" +
		"   7 0 loop0 9 9 9 9 9 9 9 9 9 9 9\n"
	writeProc(t, dir, "diskstats", first)
	if s, err := ic.collect(t0); err != nil || len(s) != 0 {
		t.Fatalf("first read: %d series, err %v; want 0", len(s), err)
	}

	const second = "   8 0 vda 1100 0 4400 140 2200 0 9000 260 0 1500 1700\n" +
		"   7 0 loop0 99 99 99 99 99 99 99 99 99 99 99\n"
	writeProc(t, dir, "diskstats", second)
	series, err := ic.collect(t0.Add(10 * time.Second))
	if err != nil {
		t.Fatal(err)
	}
	assertValues(t, values(series), map[string]float64{
		"system.io.r_s":       10,        // (1100-1000)/10
		"system.io.w_s":       20,        // (2200-2000)/10
		"system.io.rkb_s":     20,        // (4400-4000)*512/1024/10
		"system.io.wkb_s":     50,        // (9000-8000)*512/1024/10
		"system.io.util":      10,        // (1500-500)/(10*1000)*100
		"system.io.avg_q_sz":  0.1,       // (1700-700)/(10*1000)
		"system.io.await":     1.0 / 3.0, // (40+60)/(100+200) ops
		"system.io.r_await":   0.4,       // 40/100
		"system.io.w_await":   0.3,       // 60/200
		"system.io.avg_rq_sz": 14.0 / 3,  // (400+1000)/300
	})
	for _, s := range series {
		assertHasTags(t, s, "device:vda")
		for _, tag := range s.Tags {
			if tag == "device:loop0" {
				t.Error("loop device not excluded")
			}
		}
	}
}

// netDev renders an eth0 line with the given rx-bytes, rx-packets, tx-bytes,
// tx-packets (zeros elsewhere), plus an lo line that must be excluded.
func netDev(rxB, rxP, txB, txP int) string {
	return "Inter-|   Receive                    |  Transmit\n" +
		" face |bytes packets errs drop fifo frame compressed multicast|bytes packets errs drop\n" +
		"  eth0: " + cols(rxB, rxP) + cols(txB, txP) + "\n" +
		"    lo: " + cols(1, 1) + cols(1, 1) + "\n"
}

// cols renders the 8 receive (or transmit) columns: bytes packets then 6 zeros.
func cols(bytes, packets int) string {
	return strconv.Itoa(bytes) + " " + strconv.Itoa(packets) + " 0 0 0 0 0 0 "
}

// values flattens series to name->first-point value.
func values(series []metrics.Serie) map[string]float64 {
	m := make(map[string]float64, len(series))
	for _, s := range series {
		if len(s.Points) > 0 {
			m[s.Name] = s.Points[0].Value
		}
	}
	return m
}

func assertValues(t *testing.T, got, want map[string]float64) {
	t.Helper()
	for name, w := range want {
		g, ok := got[name]
		if !ok {
			t.Errorf("%s missing", name)
			continue
		}
		if math.Abs(g-w) > 1e-9 {
			t.Errorf("%s = %v, want %v", name, g, w)
		}
	}
}

func assertHasTags(t *testing.T, s metrics.Serie, want ...string) {
	t.Helper()
	for _, w := range want {
		found := false
		for _, tag := range s.Tags {
			if tag == w {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("serie %s missing tag %q (have %v)", s.Name, w, s.Tags)
		}
	}
}

func procDir(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, content := range files {
		writeProc(t, dir, name, content)
	}
	return dir
}

func writeProc(t *testing.T, dir, name, content string) {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
