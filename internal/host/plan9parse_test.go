package host

// These tests run on the dev host: the parsers carry no build constraint. The
// helpers are deliberately self-contained (no shared names) because the shared
// test helpers in host_linux_test.go only compile on linux.

import (
	"fmt"
	"math"
	"testing"
	"time"
)

// A two-CPU /dev/sysstat sample (space-padded columns, as the kernel writes them):
// id, cs, intr, syscall, pfault, tlbfault, tlbpurge, load, idle%, intr%.
const sysstatSample = "         0          0          0          0          0          0          0        250         90          5\n" +
	"         1          0          0          0          0          0          0        150         80          3\n"

// Same shape with non-zero cumulative counters, to exercise the rate columns.
const sysstatCounters = "         0       1000       2000       3000        400         50         60        250         90          5\n" +
	"         1       1100       2100       3100        410         51         61        150         80          3\n"

func TestSumSysstat(t *testing.T) {
	got := sumSysstat(parseSysstatRows(sysstatSample))
	if got.ncpu != 2 {
		t.Errorf("ncpu = %d, want 2", got.ncpu)
	}
	if got.load != 400 { // 250 + 150
		t.Errorf("load = %v, want 400", got.load)
	}
	if got.idlePct != 170 { // 90 + 80, avg = 85 across 2 CPUs
		t.Errorf("idlePct = %v, want 170", got.idlePct)
	}
	if got.intrPct != 8 { // 5 + 3
		t.Errorf("intrPct = %v, want 8", got.intrPct)
	}
	// Header/garbage/short lines are skipped.
	if g := sumSysstat(parseSysstatRows("not a row\n1 2 3\n\n")).ncpu; g != 0 {
		t.Errorf("malformed ncpu = %d, want 0", g)
	}

	// The cumulative counter columns sum across CPUs.
	c := sumSysstat(parseSysstatRows(sysstatCounters))
	if c.ctxt != 2100 || c.intr != 4100 || c.syscall != 6100 {
		t.Errorf("ctxt/intr/syscall = %v/%v/%v, want 2100/4100/6100", c.ctxt, c.intr, c.syscall)
	}
	if c.fault != 810 || c.tlbFault != 101 || c.tlbPurge != 121 {
		t.Errorf("fault/tlbFault/tlbPurge = %v/%v/%v, want 810/101/121", c.fault, c.tlbFault, c.tlbPurge)
	}
}

func TestParseCputemp(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want []float64
	}{
		{"intel", "75 4\n", []float64{75}},                            // "%ld %lud"
		{"intel alarm", "82 5 alarm\n", []float64{82}},                // trailing " alarm"
		{"amd0f", "68 1\n", []float64{68}},                            // "%ld 1"
		{"amd10 per core", "72.0 0.5\n73.0 0.5\n", []float64{72, 73}}, // "%ld.0 0.5" per core
		{"bcm bare", "58\n", []float64{58}},                           // single integer
		{"unsupported sentinel", "-1 -1 unsupported\n", nil},          // negative -> skipped
		{"empty", "", nil},
		{"mixed valid+unsupported", "70 4\n-1 -1 unsupported\n", []float64{70}},
		// amd10temprd misprints a half-degree 42.5 as "420.5 0.5" -> implausible, skipped
		{"amd10 tenfold misprint", "420.5 0.5\n", nil},
		{"mixed valid+misprint", "72.0 0.5\n420.5 0.5\n", []float64{72}},
	}
	for _, tc := range cases {
		got := parseCputemp(tc.in)
		if len(got) != len(tc.want) {
			t.Errorf("%s: len = %d %v, want %d %v", tc.name, len(got), got, len(tc.want), tc.want)
			continue
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Errorf("%s: [%d] = %v, want %v", tc.name, i, got[i], tc.want[i])
			}
		}
	}
}

func TestParseBattery(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want float64
		ok   bool
	}{
		{"apm high", "high 100 -1\n", 100, true}, // "%s %d %d" status percent time
		{"apm charging", "charging 85 73\n", 85, true},
		{"apm unknown", "unknown -1 -1\n", 0, false}, // percent -1 -> not ok
		{"bare number", "85\n", 85, true},            // bitsy /dev/battery
		{"empty", "", 0, false},
		{"no number", "nodata\n", 0, false},
	}
	for _, tc := range cases {
		got, ok := parseBattery(tc.in)
		if ok != tc.ok || (ok && got != tc.want) {
			t.Errorf("%s: parseBattery = %v,%v want %v,%v", tc.name, got, ok, tc.want, tc.ok)
		}
	}
}

func TestParseSignal(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want float64
		ok   bool
	}{
		{"wavelan", "Signal: -50\nNoise: -80\nSNR: 30\n", -50, true}, // dBm, raw-149
		{"positive", "Signal: 42\n", 42, true},
		{"not wifi", "in: 1000\nout: 2000\n", 0, false}, // non-wireless ifstats
		{"empty", "", 0, false},
	}
	for _, tc := range cases {
		got, ok := parseSignal(tc.in)
		if ok != tc.ok || (ok && got != tc.want) {
			t.Errorf("%s: parseSignal = %v,%v want %v,%v", tc.name, got, ok, tc.want, tc.ok)
		}
	}
}

func TestParseSwap(t *testing.T) {
	// 100 MiB total. 25600 pages * 4096 = 104857600. 15360 used, 10240 free.
	data := "104857600 memory\n4096 pagesize\n1024 kernel\n15360/25600 user\n5120/25600 swap\n" +
		"16777216/268435456 kernel malloc\n268435456/536870912 kernel draw\n"
	s := parseSwap(data)
	if s.memBytes != 104857600 || s.pageSize != 4096 {
		t.Errorf("mem/page = %d/%d, want 104857600/4096", s.memBytes, s.pageSize)
	}
	if s.userUsed != 15360 || s.userTotal != 25600 {
		t.Errorf("user = %d/%d, want 15360/25600", s.userUsed, s.userTotal)
	}
	if s.swapUsed != 5120 || s.swapTotal != 25600 {
		t.Errorf("swap = %d/%d, want 5120/25600", s.swapUsed, s.swapTotal)
	}
	// Two-word "kernel malloc"/"kernel draw" pools (bytes).
	if s.kmallocUsed != 16777216 || s.kmallocTotal != 268435456 {
		t.Errorf("kmalloc = %d/%d, want 16777216/268435456", s.kmallocUsed, s.kmallocTotal)
	}
	if s.kdrawUsed != 268435456 || s.kdrawTotal != 536870912 {
		t.Errorf("kdraw = %d/%d, want 268435456/536870912", s.kdrawUsed, s.kdrawTotal)
	}
	// pagesize defaults to 4096 when absent. Kernel pools stay zero when absent.
	bare := parseSwap("104857600 memory\n")
	if bare.pageSize != 4096 {
		t.Error("missing pagesize should default to 4096")
	}
	if bare.kmallocTotal != 0 || bare.kdrawTotal != 0 {
		t.Error("absent kernel pools should be zero")
	}
}

// TestSwapSeries pins the unit conversion in swapSeries against a real /dev/swap
// (from a 3 GiB amd64 host with NO swap attached). The whole point is that
// /dev/swap mixes units: this catches a regression that forgot to scale the
// page-counted "user" field by pageSize, or that wrongly scaled the byte-counted
// "memory"/kernel pools. It also pins the no-swap fix: the file's "0/160000 swap"
// is the kernel's boot-time slot ceiling, present with no store attached, so with
// swapCapBytes=0 (no /env/swap) the swap gauges MUST read 0, not the phantom 625
// MB (160000*4096) that deriving total from /dev/swap would yield.
func TestSwapSeries(t *testing.T) {
	// Exactly what the kernel writes (sys/src/9/port/devcons.c Qswap): memory in
	// bytes, user/swap in pages, kernel malloc/draw in bytes.
	const data = "3216392192 memory\n4096 pagesize\n52139 kernel\n" +
		"333274/733113 user\n0/160000 swap\n" +
		"20583456/112266496 kernel malloc\n0/16777216 kernel draw\n"
	got := map[string]float64{}
	for _, s := range swapSeries(parseSwap(data), 0, time.Unix(0, 0)) { // no /env/swap -> capacity 0
		got[s.Name] = s.Points[0].Value
	}

	// want values in MB, tol is the allowed absolute error. The page-scaled field
	// (mem.free/used ~1562/1505) would be ~4096x too small if the *pageSize were
	// dropped. The byte fields (mem.total ~3067, kernel pools) would be ~4096x too
	// large if pageSize were wrongly applied. The swap gauges are all 0: no store.
	for _, c := range []struct {
		name string
		want float64
		tol  float64
	}{
		{"system.mem.total", 3067.39, 0.1},       // bytes, NOT scaled
		{"system.mem.free", 1561.87, 0.1},        // user pages -> bytes
		{"system.mem.used", 1505.52, 0.1},        // total - free
		{"system.mem.pct_usable", 0.5092, 0.001}, // free/total ratio
		{"system.swap.total", 0.0, 0.001},        // NO swap: capacity 0, NOT the 625 MB ceiling
		{"system.swap.used", 0.0, 0.001},
		{"system.swap.free", 0.0, 0.001},
		{"system.mem.kernel.malloc", 19.63, 0.01}, // bytes, NOT scaled
		{"system.mem.kernel.malloc.max", 107.07, 0.01},
		{"system.mem.kernel.draw", 0.0, 0.001},
		{"system.mem.kernel.draw.max", 16.0, 0.001}, // bytes, NOT scaled (exact)
	} {
		v, ok := got[c.name]
		if !ok {
			t.Errorf("%s: missing from emitted series", c.name)
			continue
		}
		if math.Abs(v-c.want) > c.tol {
			t.Errorf("%s = %.4f, want %.4f ±%g", c.name, v, c.want, c.tol)
		}
	}
}

// TestSwapSeriesConfigured pins the configured-swap path: capacity comes from the
// collector's stat of the /env/swap backing store (swapCapBytes), while "used"
// comes from the truthful left field of the /dev/swap "swap" line scaled by
// pageSize. The 160000-page ceiling in the file must be ignored entirely.
func TestSwapSeriesConfigured(t *testing.T) {
	// 12800 pages used * 4096 = 50 MiB used. The 160000 ceiling is a red herring.
	const data = "3216392192 memory\n4096 pagesize\n52139 kernel\n" +
		"333274/733113 user\n12800/160000 swap\n"
	const capBytes = 256 * 1024 * 1024 // 256 MiB backing store (stat of /env/swap target)
	got := map[string]float64{}
	for _, s := range swapSeries(parseSwap(data), capBytes, time.Unix(0, 0)) {
		got[s.Name] = s.Points[0].Value
	}
	for _, c := range []struct {
		name string
		want float64
	}{
		{"system.swap.total", 256.0}, // from capacity, NOT 625 (the ceiling)
		{"system.swap.used", 50.0},   // 12800 pages * 4096 -> 50 MiB
		{"system.swap.free", 206.0},  // 256 - 50
	} {
		if math.Abs(got[c.name]-c.want) > 0.001 {
			t.Errorf("%s = %.4f, want %.4f", c.name, got[c.name], c.want)
		}
	}
}

// TestSwap9kFormat pins compatibility with the experimental 64-bit "9k" kernel,
// whose /dev/swap differs from the stock 9/port kernel in four ways (sys/src/9k/
// port/devcons.c Qswap + qmalloc.c mallocreadfmt, appended via mallocreadsummary):
//  1. memory is "%llud" (64-bit) so it can exceed 4 GiB. The value below is 8 GiB,
//     larger than a uint32, which would truncate if parsed as 32-bit.
//  2. swap is hardcoded "0/0" (9k has no swap subsystem. The line is kept only to
//     "keep old 9 scripts happy"), so swapUsed/swapTotal parse as 0, and capacity
//     comes from /env/swap as always.
//  3. "kernel draw" is hardcoded "0/0", so its gauges must be omitted.
//  4. two extra trailing lines ("quick:"/"rover:") that no other kernel emits and
//     that the parser must ignore without corrupting the real fields.
//
// "kernel malloc" IS real on 9k (from mallocreadfmt), so its gauges are emitted.
func TestSwap9kFormat(t *testing.T) {
	// Exactly what a 9k kernel writes on an 8 GiB host (memory bytes, user pages,
	// kernel malloc bytes), including the appended quick:/rover: summary lines.
	const data = "8589934592 memory\n4096 pagesize\n65536 kernel\n" +
		"524288/2031616 user\n0/0 swap\n" +
		"3014656/16777216 kernel malloc\n0/0 kernel draw\n" +
		"quick: 49152 bytes total\nrover: 3 blocks 98304 bytes total\n"

	// Parse level: the 8 GiB value survives (would be 4294967296 short if truncated
	// to uint32), swap is 0/0, draw is 0/0, and the quick:/rover: lines are ignored.
	s := parseSwap(data)
	if s.memBytes != 8589934592 {
		t.Errorf("memBytes = %d, want 8589934592 (64-bit value must not truncate)", s.memBytes)
	}
	if s.userUsed != 524288 || s.userTotal != 2031616 {
		t.Errorf("user = %d/%d, want 524288/2031616", s.userUsed, s.userTotal)
	}
	if s.swapUsed != 0 || s.swapTotal != 0 {
		t.Errorf("swap = %d/%d, want 0/0 (9k hardcodes it)", s.swapUsed, s.swapTotal)
	}
	if s.kmallocUsed != 3014656 || s.kmallocTotal != 16777216 {
		t.Errorf("kmalloc = %d/%d, want 3014656/16777216 (real on 9k)", s.kmallocUsed, s.kmallocTotal)
	}
	if s.kdrawTotal != 0 {
		t.Errorf("kdrawTotal = %d, want 0 (9k hardcodes draw to 0/0)", s.kdrawTotal)
	}

	// Series level: clean exact values (free = (2031616-524288)*4096 = 5888 MiB).
	got := map[string]float64{}
	for _, ser := range swapSeries(s, 0, time.Unix(0, 0)) { // no /env/swap -> capacity 0
		got[ser.Name] = ser.Points[0].Value
	}
	for _, c := range []struct {
		name string
		want float64
	}{
		{"system.mem.total", 8192.0},       // 8 GiB, NOT scaled by pageSize
		{"system.mem.free", 5888.0},        // user free pages -> bytes
		{"system.mem.used", 2304.0},        // total - free
		{"system.mem.pct_usable", 0.71875}, // 5888/8192
		{"system.swap.total", 0.0},         // 0/0 swap + no /env/swap
		{"system.swap.used", 0.0},
		{"system.swap.free", 0.0},
		{"system.mem.kernel.malloc", 2.875}, // bytes, NOT scaled
		{"system.mem.kernel.malloc.max", 16.0},
	} {
		v, ok := got[c.name]
		if !ok {
			t.Errorf("%s: missing from emitted series", c.name)
			continue
		}
		if math.Abs(v-c.want) > 0.001 {
			t.Errorf("%s = %.5f, want %.5f", c.name, v, c.want)
		}
	}
	// draw is 0/0 and capacity is 0, so these gauges must NOT be emitted.
	for _, absent := range []string{
		"system.mem.kernel.draw", "system.mem.kernel.draw.max", "system.swap.pct_free",
	} {
		if _, ok := got[absent]; ok {
			t.Errorf("%s emitted, want absent on 9k/no-swap", absent)
		}
	}
}

func TestParseUptime(t *testing.T) {
	// fastticks/fasthz = 360000/1000 = 360s.
	if got := parseUptime("1700000000 1700000000000000000 360000 1000 999\n"); math.Abs(got-360) > 1e-9 {
		t.Errorf("uptime = %v, want 360", got)
	}
	if got := parseUptime("1 2"); got != 0 { // too few fields
		t.Errorf("short uptime = %v, want 0", got)
	}
	if got := parseUptime("1 2 3 0"); got != 0 { // fasthz 0 guarded
		t.Errorf("zero-hz uptime = %v, want 0", got)
	}
}

func TestParseEtherStats(t *testing.T) {
	data := "in: 1500\nlink: 1\nout: 2500\ncrc errs: 0\noverflows: 0\naddr: 080027000b5d\n"
	s := parseEtherStats(data)
	if !s.hasIn || s.inPkts != 1500 {
		t.Errorf("in = %v (has=%v), want 1500", s.inPkts, s.hasIn)
	}
	if !s.hasOut || s.outPkts != 2500 {
		t.Errorf("out = %v (has=%v), want 2500", s.outPkts, s.hasOut)
	}

	// Error counters (summed), link, and speed.
	data2 := "in: 100\nout: 200\nlink: 1\ncrc errs: 3\noverflows: 2\nsoft overflows: 1\n" +
		"framing errs: 4\nbuffer errs: 5\noutput errs: 6\nmbps: 1000\naddr: 001122334455\n"
	s2 := parseEtherStats(data2)
	if got := s2.errTotal(); got != 21 { // 3+2+1+4+5+6
		t.Errorf("errTotal = %v, want 21", got)
	}
	if !s2.hasLink || s2.link != 1 {
		t.Errorf("link = %v (has=%v), want 1", s2.link, s2.hasLink)
	}
	if !s2.hasMbps || s2.mbps != 1000 {
		t.Errorf("mbps = %v (has=%v), want 1000", s2.mbps, s2.hasMbps)
	}
}

// TestParseNetStatsTCP pins /net/tcp/stats (sys/src/9/ip/tcp.c) parsing through the
// same generic "Label: value" parser the other protocol stats use. The netProtoSources
// table maps these labels to system.net.tcp.*: CurrEstab and InLimbo are instantaneous
// (gauges), the rest are cumulative counters (rates).
func TestParseNetStatsTCP(t *testing.T) {
	data := "MaxConn: 4000\nMss: 1460\nActiveOpens: 100\nPassiveOpens: 50\nEstabResets: 3\n" +
		"CurrEstab: 12\nInSegs: 9000\nOutSegs: 8000\nRetransSegs: 7\nRetransTimeouts: 2\n" +
		"InErrs: 1\nOutRsts: 9\nInLimbo: 4\nnonsense\nNoNumber: x\n"
	m := parseNetStats(data)
	want := map[string]float64{
		"CurrEstab": 12, "InLimbo": 4,
		"InSegs": 9000, "OutSegs": 8000, "RetransSegs": 7, "RetransTimeouts": 2,
		"EstabResets": 3, "InErrs": 1, "ActiveOpens": 100, "PassiveOpens": 50, "OutRsts": 9,
	}
	for label, v := range want {
		if m[label] != v {
			t.Errorf("%s = %v, want %v", label, m[label], v)
		}
	}
	if _, ok := m["NoNumber"]; ok {
		t.Error("non-numeric value should be skipped")
	}
}

func TestParseProcStatus(t *testing.T) {
	// devproc.c Qstatus layout: name and user in KNAMELEN(28)-wide space-padded
	// columns, state in a 12-wide column, then nine numbers each right-justified
	// in a 12-wide column (t0..t5, mem kB, basepri, pri).
	line := func(name, user, state string, nums ...int) string {
		s := fmt.Sprintf("%-28s%-28s%-12s", name, user, state)
		for _, n := range nums {
			s += fmt.Sprintf("%11d ", n)
		}
		return s
	}
	ps, ok := parseProcStatus(line("webfs", "eve", "Wakeme", 10, 20, 5, 0, 0, 0, 4096, 10, 10))
	if !ok || ps.name != "webfs" || ps.state != "Wakeme" || ps.memKB != 4096 {
		t.Errorf("parseProcStatus = %+v ok=%v, want {webfs Wakeme 4096} true", ps, ok)
	}
	// A program name containing a space stays intact: the text columns are fixed
	// width, so it cannot shift the later fields.
	ps, ok = parseProcStatus(line("factotum helper", "eve", "Idle", 1, 2, 3, 0, 0, 0, 512, 10, 10))
	if !ok || ps.name != "factotum helper" || ps.state != "Idle" || ps.memKB != 512 {
		t.Errorf("spaced name = %+v ok=%v, want {factotum helper Idle 512} true", ps, ok)
	}
	if _, ok := parseProcStatus("too few fields here"); ok {
		t.Error("short status line should not parse")
	}
}

func TestTopProcsByMem(t *testing.T) {
	procs := []procStatus{{name: "a", memKB: 100}, {name: "b", memKB: 50}, {name: "a", memKB: 25}, {name: "c", memKB: 200}}
	top := topProcsByMem(procs, 2) // a aggregates to 125. Ranked c(200), a(125), b(50)
	if len(top) != 2 {
		t.Fatalf("len = %d, want 2", len(top))
	}
	if top[0].name != "c" || top[0].memKB != 200 {
		t.Errorf("top[0] = %+v, want {c 200}", top[0])
	}
	if top[1].name != "a" || top[1].memKB != 125 {
		t.Errorf("top[1] = %+v, want {a 125}", top[1])
	}
}

func TestProcShareCount(t *testing.T) {
	// /proc/N/segment: "type [R] base top ref". The address-space share count is the
	// Data/Bss refcount. Text (image-cache shared between unrelated procs) and Stack
	// (private) are ignored. Here Data/Bss are shared by 14 rfork(RFMEM) threads.
	seg := "Text   R  00001000 006b4000 14\n" +
		"Data      006b4000 00707000 14\n" +
		"Bss       00707000 03800000 14\n" +
		"Stack     defff000 dffff000 1\n"
	if n := procShareCount(seg); n != 14 {
		t.Errorf("procShareCount = %d, want 14", n)
	}
	// Text shared (ref 2, image cache) but Data/Bss private -> independent procs, count 1.
	indep := "Text   R  00001000 0002e000 2\nData 0002e000 0003d000 1\nBss 0003d000 00069000 1\n"
	if n := procShareCount(indep); n != 1 {
		t.Errorf("procShareCount(indep) = %d, want 1 (Text sharing must not count)", n)
	}
	// No Data/Bss line, empty, or garbage -> 1 (no division).
	if n := procShareCount(""); n != 1 {
		t.Errorf("procShareCount(empty) = %d, want 1", n)
	}
	if n := procShareCount("Stack 00002000\nbad"); n != 1 {
		t.Errorf("procShareCount(garbage) = %d, want 1", n)
	}

	// End to end: 14 threads each reporting 57344 kB, divided by the share count and
	// summed by program, count the shared image once (57344), not 14× (802816).
	const mem = 57344.0 // = 14 * 4096, so the division is exact
	share := float64(procShareCount(seg))
	procs := make([]procStatus, 14)
	for i := range procs {
		procs[i] = procStatus{name: "agent", memKB: mem / share}
	}
	if top := topProcsByMem(procs, 1); len(top) != 1 || top[0].memKB != mem {
		t.Errorf("deduped agent mem = %+v, want one {agent %g}", top, mem)
	}
}

func TestParseSysstatRows(t *testing.T) {
	rows := parseSysstatRows(sysstatCounters)
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2", len(rows))
	}
	if rows[0].id != 0 || rows[0].ctxt != 1000 || rows[0].idlePct != 90 || rows[0].intrPct != 5 {
		t.Errorf("row0 = %+v", rows[0])
	}
	if rows[1].id != 1 || rows[1].syscall != 3100 || rows[1].fault != 410 || rows[1].load != 150 {
		t.Errorf("row1 = %+v", rows[1])
	}
	// Summing the rows must match the aggregate (rate columns).
	if g := sumSysstat(rows); g.ctxt != rows[0].ctxt+rows[1].ctxt || g.ncpu != 2 {
		t.Errorf("aggregate ctxt=%v ncpu=%d disagrees with rows", g.ctxt, g.ncpu)
	}
}

func TestCountConnStates(t *testing.T) {
	// /net/tcp/<n>/status: state is the first field (sys/src/9/ip/tcp.c tcpstate).
	statuses := []string{
		"Established qin 0 qout 0 rq 0.0 srtt 5 mdev 2",
		"Established qin 0 qout 0 rq 0.0",
		"Listen qin 0 qout 0",
		"Time_wait qin 0",
		"", // closed/empty conv: ignored
	}
	c := countConnStates(statuses)
	if c["established"] != 2 || c["listen"] != 1 || c["time_wait"] != 1 {
		t.Errorf("counts = %v, want established:2 listen:1 time_wait:1", c)
	}
	if connState("Syn_sent x y") != "Syn_sent" || connState("") != "" {
		t.Error("connState mismatch")
	}
}

func TestParseNetStats(t *testing.T) {
	// IP/ICMP/UDP stats are "Label: value". ICMP appends per-type "Echo: 5 3" rows
	// (two values) that must be skipped.
	data := "InReceives: 1000\nInHdrErrors: 2\nForwDatagrams: 0\nReasmFails: 1\n" +
		"EchoReply: 5 3\nnonsense\nBad: x\n"
	m := parseNetStats(data)
	if m["InReceives"] != 1000 || m["InHdrErrors"] != 2 || m["ReasmFails"] != 1 {
		t.Errorf("parseNetStats = %v", m)
	}
	if _, ok := m["EchoReply"]; ok {
		t.Error("two-value per-type line should be skipped")
	}
	if _, ok := m["Bad"]; ok {
		t.Error("non-numeric value should be skipped")
	}
}

func TestParseIpifcStatus(t *testing.T) {
	// The header line is a flat "key value …" list (sys/src/9/ip/ipifc.c).
	const data = "device /net/ether0 maxtu 1514 sendra 0 recvra 0 mflag 0 oflag 0 " +
		"maxraint 600000 minraint 198000 linkmtu 1514 reachtime 0 rxmitra 0 ttl 0 routerlt 0 " +
		"pktin 12345 pktout 6789 errin 1 errout 2\n" +
		"\t10.0.0.1 /128 10.0.0.0 0 0\n"
	s, ok := parseIpifcStatus(data)
	if !ok || s.device != "/net/ether0" || s.mtu != 1514 {
		t.Errorf("parseIpifcStatus = %+v ok=%v", s, ok)
	}
	if s.pktin != 12345 || s.pktout != 6789 || s.errin != 1 || s.errout != 2 {
		t.Errorf("counters = %+v", s)
	}
	if _, ok := parseIpifcStatus(""); ok {
		t.Error("empty status should not parse")
	}
}

func TestProcStateBucket(t *testing.T) {
	cases := map[string]string{
		"Running": "running", "Scheding": "running", "Ready": "ready", "New": "ready",
		"Queueing": "blocked", "QueueingW": "blocked", "Stopped": "stopped",
		"Moribund": "zombie", "Dead": "zombie", "Broken": "broken",
		"Fault": "sleeping", "Pageout": "sleeping", "Wakeme": "sleeping", "Anything": "sleeping",
	}
	for state, want := range cases {
		if got := procStateBucket(state); got != want {
			t.Errorf("procStateBucket(%q) = %q, want %q", state, got, want)
		}
	}
}

func TestParseIfstats(t *testing.T) {
	// ether82557: "Label: value" counter lines, plus multi-value eeprom/phy dumps that
	// carry no single number and must be skipped.
	i82557 := "transmit good frames: 2847\nreceive CRC errors: 5\nnop: 0\n" +
		"eeprom: 8086 0100 0000 0000\nphy  0: 1000 7849 0043 05e1\n"
	m := parseIfstats(i82557)
	if m["transmit good frames"] != 2847 || m["receive CRC errors"] != 5 || m["nop"] != 0 {
		t.Errorf("82557 counters = %v", m)
	}
	if _, ok := m["eeprom"]; ok {
		t.Error("eeprom (multi-value) should be skipped")
	}
	if len(m) != 3 {
		t.Errorf("82557 len = %d, want 3 (%v)", len(m), m)
	}

	// ether8169: short names. The hex tcr/rcr lines are not plain floats and are skipped.
	m = parseIfstats("TxOk: 15234\nRxEr: 0\ntcr: 0x00072400\nmulticast: 1\n")
	if m["TxOk"] != 15234 || m["RxEr"] != 0 || m["multicast"] != 1 {
		t.Errorf("8169 counters = %v", m)
	}
	if _, ok := m["tcr"]; ok {
		t.Error("hex tcr should be skipped")
	}

	// virtio-net ifstats has no "Label: value" lines (per-queue dumps only) -> empty.
	virtio := "devfeat 000f\ndevstatus 07\nvq0 0x123 size 256 avail->idx 0 nintr 120 nnote 8\n"
	if m := parseIfstats(virtio); len(m) != 0 {
		t.Errorf("virtio ifstats = %v, want empty", m)
	}

	// Signal is excluded from the counter map (instantaneous, shipped as the
	// system.net.wifi.signal gauge via parseSignal).
	if m := parseIfstats("Signal: -50\nin: 100\n"); m["in"] != 100 || len(m) != 1 {
		t.Errorf("signal-excluded = %v", m)
	}
}

func TestIfstatMetric(t *testing.T) {
	cases := map[string]string{
		"receive CRC errors":        "receive_crc_errors",
		"TxOk":                      "txok",
		"transmit good frames":      "transmit_good_frames",
		"transmit total collisions": "transmit_total_collisions",
		"mtu":                       "mtu_stat", // reserved: collides with the ipifc gauge
		"up":                        "up_stat",
		"":                          "unknown",
	}
	for in, want := range cases {
		if got := ifstatMetric(in); got != want {
			t.Errorf("ifstatMetric(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestParseTimesync(t *testing.T) {
	// syslog-prefixed δ line. The δ/avgδ/hz glyphs are matched as UTF-8 tokens.
	one := "cetus Jun 23 10:00:00 δ -1234 avgδ 5678 hz 1000000000\n"
	s, ok := parseTimesync(one)
	if !ok || s.offset != -1234 || s.avgOffset != 5678 || s.freq != 1000000000 {
		t.Errorf("parseTimesync = %+v ok=%v", s, ok)
	}

	// The last complete sample wins. "no sample"/"can't reach" lines are ignored.
	multi := "cetus Jun 23 09:00:00 δ 1 avgδ 2 hz 999\n" +
		"cetus Jun 23 09:30:00 no sample\n" +
		"cetus Jun 23 10:00:00 δ -42 avgδ 7 hz 1000000000\n"
	if s, ok := parseTimesync(multi); !ok || s.offset != -42 || s.avgOffset != 7 {
		t.Errorf("parseTimesync(last) = %+v ok=%v", s, ok)
	}

	for _, neg := range []string{"", "cetus Jun 23 10:00:00 no sample\n", "cetus Jun 23 10:00:00 can't reach ntp.example.com: timed out\n"} {
		if _, ok := parseTimesync(neg); ok {
			t.Errorf("parseTimesync(%q) should not parse", neg)
		}
	}
}
