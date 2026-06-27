package host

// Pure parsers for Plan 9's text-based /dev and /net statistics files. This file
// carries no build constraint (its name has no _GOOS suffix), so the parsers
// compile and are unit-tested on the dev host. The glue that opens the real
// kernel files lives in collectors_plan9.go (compiled only on plan9).

import (
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/0intro/dd-micro-agent/internal/metrics"
)

// sysstatTotals aggregates /dev/sysstat, which the kernel writes one line per
// online CPU (sys/src/9/port/devcons.c, Qsysstat) with ten space-padded columns:
// id, context switches, interrupts, syscalls, page faults, tlb faults, tlb purges,
// load, idle %, interrupt %. load/idle/interrupt % are already instantaneous and
// feed gauges directly. The counter columns (1-6) are cumulative and feed rate
// collectors that diff two reads.
type sysstatTotals struct {
	ncpu     int
	load     float64 // summed load (col 7), per-mille: 1000 == one fully-busy CPU
	idlePct  float64 // summed idle % (col 8)
	intrPct  float64 // summed interrupt % (col 9), instantaneous
	ctxt     float64 // summed context switches (col 1), cumulative counter
	intr     float64 // summed interrupts (col 2), cumulative
	syscall  float64 // summed syscalls (col 3), cumulative
	fault    float64 // summed page faults (col 4), cumulative
	tlbFault float64 // summed tlb faults (col 5), cumulative
	tlbPurge float64 // summed tlb purges (col 6), cumulative (→ system.cpu.tlb_purges)
}

// sysstatRow is one /dev/sysstat line (one online CPU). The column meanings match
// sysstatTotals. id is the kernel's CPU number (col 0).
type sysstatRow struct {
	id                                             int
	ctxt, intr, syscall, fault, tlbFault, tlbPurge float64 // cumulative counters
	load, idlePct, intrPct                         float64 // instantaneous
}

// parseSysstatRows returns one row per online CPU. A row must carry all ten numeric
// columns. Header/partial/garbage lines are skipped.
func parseSysstatRows(data string) []sysstatRow {
	var rows []sysstatRow
	for _, line := range strings.Split(data, "\n") {
		f := strings.Fields(line)
		if len(f) < 10 {
			continue
		}
		var n [10]float64
		ok := true
		for i := 0; i < 10; i++ {
			v, err := strconv.ParseFloat(f[i], 64)
			if err != nil {
				ok = false
				break
			}
			n[i] = v
		}
		if !ok {
			continue
		}
		rows = append(rows, sysstatRow{
			id: int(n[0]), ctxt: n[1], intr: n[2], syscall: n[3], fault: n[4],
			tlbFault: n[5], tlbPurge: n[6], load: n[7], idlePct: n[8], intrPct: n[9],
		})
	}
	return rows
}

// sumSysstat sums per-CPU rows into host totals.
func sumSysstat(rows []sysstatRow) sysstatTotals {
	var t sysstatTotals
	for _, r := range rows {
		t.ctxt += r.ctxt
		t.intr += r.intr
		t.syscall += r.syscall
		t.fault += r.fault
		t.tlbFault += r.tlbFault
		t.tlbPurge += r.tlbPurge
		t.load += r.load
		t.idlePct += r.idlePct
		t.intrPct += r.intrPct
		t.ncpu++
	}
	return t
}

// parseCputemp reads /dev/cputemp, whose format is arch-dependent (sys/src/9/pc/devarch.c,
// bcm/devarch.c): one line per thermal sensor, the first field a Celsius temperature
// ("75 4", "68 1", "72.0 0.5", or a bare "58"). The "no sensor" sentinel is
// "-1 -1 unsupported". Returns the per-sensor temperatures, skipping lines whose first
// field is missing, non-numeric, negative (unsupported), or above 150 (implausible for
// a CPU: the AMD family-10h reader misprints half-degree readings tenfold, seprint
// "%ld%s" with the fraction string "0.5" turning 42.5 into "420.5 0.5", devarch.c
// amd10temprd).
func parseCputemp(data string) []float64 {
	var out []float64
	for _, line := range strings.Split(data, "\n") {
		f := strings.Fields(line)
		if len(f) == 0 {
			continue
		}
		t, err := strconv.ParseFloat(f[0], 64)
		if err != nil || t < 0 || t > 150 {
			continue
		}
		out = append(out, t)
	}
	return out
}

// parseBattery extracts the battery charge percentage from /mnt/apm/battery, whose first
// line is "<status> <percent> <time>" (aux/apm.c: snprint "%s %d %d"), e.g. "high 100 -1"
// or "charging 85 73". Percent is -1 when unknown. The bitsy /dev/battery is a bare
// number, also handled. Returns the first non-negative integer on the first line, with
// ok=false for no reading / unknown.
func parseBattery(data string) (float64, bool) {
	line := data
	if i := strings.IndexByte(line, '\n'); i >= 0 {
		line = line[:i]
	}
	for _, f := range strings.Fields(line) {
		v, err := strconv.ParseFloat(f, 64)
		if err != nil {
			continue // skip the leading status word
		}
		if v < 0 {
			return 0, false // unknown (-1)
		}
		return v, true
	}
	return 0, false
}

// parseSignal extracts the 802.11 signal level (dBm) from a /net/ether<n>/ifstats whose
// first line is "Signal: <dBm>" (sys/src/9/pc/wavelan.c, the value is already raw-149, so
// it is typically negative). Only wavelan-style interfaces start with "Signal: ".
// Non-wireless ifstats return ok=false.
func parseSignal(data string) (float64, bool) {
	line := data
	if i := strings.IndexByte(line, '\n'); i >= 0 {
		line = line[:i]
	}
	const pfx = "Signal: "
	if !strings.HasPrefix(line, pfx) {
		return 0, false
	}
	v, err := strconv.ParseFloat(strings.TrimSpace(line[len(pfx):]), 64)
	if err != nil {
		return 0, false
	}
	return v, true
}

// parseIfstats returns every numeric "Label: value" line from a /net/etherN/ifstats file,
// keyed by the trimmed label. ifstats content is driver-specific (sys/src/9/*/ether*.c
// ifstat): physical NICs dump rich cumulative counters (ether82557 "receive CRC errors",
// ether8169 "TxOk", …) while virtio-net exposes only per-queue lines that carry no colon
// and are skipped. "Signal" is omitted from the counter map because it is an instantaneous
// dBm level, which plan9Ifstats ships as the system.net.wifi.signal gauge via parseSignal.
// Multi-value lines (eeprom/phy hex dumps) parse to no number and are ignored.
func parseIfstats(data string) map[string]float64 {
	out := map[string]float64{}
	for _, line := range strings.Split(data, "\n") {
		label, val, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		label = strings.TrimSpace(label)
		if label == "" || label == "Signal" {
			continue
		}
		v, err := strconv.ParseFloat(strings.TrimSpace(val), 64)
		if err != nil {
			continue
		}
		out[label] = v
	}
	return out
}

// ifstatMetric turns a driver ifstats label ("receive CRC errors", "TxOk") into a metric
// suffix ("receive_crc_errors", "txok"): lowercase, runs of non-alphanumerics collapse to a
// single underscore. The few suffixes already used by the ipifc/stats-derived
// system.net.iface.* gauges get a "_stat" suffix so they can't collide.
func ifstatMetric(label string) string {
	var b strings.Builder
	under := false
	for _, r := range strings.ToLower(label) {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			b.WriteRune(r)
			under = false
		} else if !under {
			b.WriteByte('_')
			under = true
		}
	}
	s := strings.Trim(b.String(), "_")
	switch s {
	case "":
		return "unknown"
	case "mtu", "up", "speed_mbps", "packets_in", "packets_out", "errors_in", "errors_out":
		return s + "_stat"
	}
	return s
}

// swapInfo holds the fields we use from /dev/swap (sys/src/9/port/devcons.c,
// Qswap). Relevant lines: "<bytes> memory", "<bytes> pagesize",
// "<used>/<total> user" and "<used>/<total> swap" (user/swap in pages), plus the
// kernel allocator pools "<cur>/<max> kernel malloc" and "... kernel draw" (bytes).
//
// The experimental 64-bit "9k" kernel (sys/src/9k/port/devcons.c + qmalloc.c) is
// handled by the same parser: memory is "%llud" so it may exceed 4 GiB (parsed as
// uint64), "swap" and "kernel draw" are hardcoded "0/0", "kernel malloc" is real,
// and two extra trailing lines ("quick:"/"rover:") appear. These match no label
// and are ignored. See TestSwap9kFormat.
type swapInfo struct {
	memBytes  uint64 // total physical memory, bytes
	pageSize  uint64 // bytes per page
	userUsed  uint64 // user pages in use
	userTotal uint64 // user pages total
	swapUsed  uint64 // swap pages in use (truthful: pages actually paged out, 0 == no swap)
	swapTotal uint64 // swap-slot ceiling in pages (conf.nproc*80 at boot, NOT real capacity, see swapSeries)

	kmallocUsed, kmallocTotal uint64 // kernel malloc pool, bytes
	kdrawUsed, kdrawTotal     uint64 // kernel draw (image) pool, bytes
}

// parseSwap reads the lines we care about and ignores the rest. The "kernel
// malloc"/"kernel draw" pools carry a two-word trailing label, so they're matched
// before the single-word switch (which keys on the last word alone).
func parseSwap(data string) swapInfo {
	var s swapInfo
	for _, line := range strings.Split(data, "\n") {
		f := strings.Fields(line)
		if len(f) < 2 {
			continue
		}
		if len(f) >= 3 {
			switch f[len(f)-2] + " " + f[len(f)-1] { // two-word label
			case "kernel malloc":
				s.kmallocUsed, s.kmallocTotal = parseFraction(f[0])
				continue
			case "kernel draw":
				s.kdrawUsed, s.kdrawTotal = parseFraction(f[0])
				continue
			}
		}
		switch f[len(f)-1] { // the label is the trailing word
		case "memory":
			s.memBytes = parseUint(f[0])
		case "pagesize":
			s.pageSize = parseUint(f[0])
		case "user":
			s.userUsed, s.userTotal = parseFraction(f[0])
		case "swap":
			s.swapUsed, s.swapTotal = parseFraction(f[0])
		}
	}
	if s.pageSize == 0 {
		s.pageSize = 4096
	}
	return s
}

// swapSeries turns a parsed /dev/swap into the memory/swap gauges (MB). It lives
// here, split from the collector, because /dev/swap mixes units and the conversion
// is the easy thing to get wrong: "memory" and the kernel malloc/draw pools are
// already bytes, but "user" and "swap" are page counts (kernel format
// "%lud memory"=npage*BY2PG, "%lud/%lud user"=pages, "%lud/%lud kernel malloc"=
// bytes, sys/src/9/port/devcons.c Qswap), so the page fractions (and only those)
// must be multiplied by pageSize. Keeping this pure lets the unit handling be
// pinned by a test on the dev host. Caller guarantees s.memBytes > 0.
//
// swapCapBytes is the true swap capacity (the size of the backing store named in
// /env/swap, 0 when none) supplied by the collector. It is deliberately NOT
// derived from /dev/swap's "swap" total field (s.swapTotal): that field is the
// kernel's boot-time slot-map ceiling (conf.nproc*80 pages, ~625 MB on a default
// kernel), reported whether or not a store is attached, so using it would surface
// a phantom 625 MB swap on a host with none. The "used" (left) field is kept as-is:
// it is always truthful (pages actually paged out), so it reads 0 when there's no
// swap and pairs correctly with the stat'd capacity.
func swapSeries(s swapInfo, swapCapBytes uint64, now time.Time) []metrics.Serie {
	const mb = 1024.0 * 1024.0

	// Free user pages = total - used. Everything else (kernel) counts as used.
	// user is in PAGES, so scale to bytes before comparing against memBytes.
	var free uint64
	if s.userTotal >= s.userUsed {
		free = (s.userTotal - s.userUsed) * s.pageSize
	}
	if free > s.memBytes {
		free = s.memBytes
	}
	used := s.memBytes - free

	swapUsed := s.swapUsed * s.pageSize // swap "used" is in PAGES
	if swapUsed > swapCapBytes {
		swapUsed = swapCapBytes // used can't exceed capacity, guards the free subtraction
	}

	out := []metrics.Serie{
		gauge("system.mem.total", now, float64(s.memBytes)/mb),
		gauge("system.mem.free", now, float64(free)/mb),
		gauge("system.mem.used", now, float64(used)/mb),
		gauge("system.mem.pct_usable", now, float64(free)/float64(s.memBytes)),
		gauge("system.swap.total", now, float64(swapCapBytes)/mb), // 0 when no swap configured
		gauge("system.swap.used", now, float64(swapUsed)/mb),
		gauge("system.swap.free", now, float64(swapCapBytes-swapUsed)/mb),
	}
	if swapCapBytes > 0 { // 0 to 1 ratio, like system.mem.pct_usable, omitted when no swap
		out = append(out, gauge("system.swap.pct_free", now, float64(swapCapBytes-swapUsed)/float64(swapCapBytes)))
	}
	// Kernel allocator pools are already BYTES -> MB, current use + ceiling. Emitted
	// only when the kernel reports them (present on amd64, absent on older kernels).
	if s.kmallocTotal > 0 {
		out = append(out,
			gauge("system.mem.kernel.malloc", now, float64(s.kmallocUsed)/mb),
			gauge("system.mem.kernel.malloc.max", now, float64(s.kmallocTotal)/mb),
		)
	}
	if s.kdrawTotal > 0 {
		out = append(out,
			gauge("system.mem.kernel.draw", now, float64(s.kdrawUsed)/mb),
			gauge("system.mem.kernel.draw.max", now, float64(s.kdrawTotal)/mb),
		)
	}
	return out
}

// parseUptime derives uptime seconds from /dev/time, whose format
// (sys/src/9/port/devcons.c, readtime) is "sec nsec fastticks fasthz [mono]":
// uptime is fastticks / fasthz.
func parseUptime(data string) float64 {
	f := strings.Fields(data)
	if len(f) < 4 {
		return 0
	}
	ticks, _ := strconv.ParseFloat(f[2], 64)
	hz, _ := strconv.ParseFloat(f[3], 64)
	if hz <= 0 {
		return 0
	}
	return ticks / hz
}

// clockSample is the most recent clock-discipline reading from aux/timesync.
type clockSample struct {
	offset    float64 // last offset to the time source, nanoseconds
	avgOffset float64 // averaged offset, nanoseconds
	freq      float64 // disciplined fast-clock frequency, Hz
}

// parseTimesync extracts the latest clock-drift sample from /sys/log/timesync. aux/timesync
// logs "δ <offset> avgδ <avg> hz <freq>" per sample (sys/src/cmd/aux/timesync.c), prefixed
// by syslog with "sysname Mon DD HH:MM:SS ". It is matched by the "δ"/"avgδ"/"hz" tokens
// (Plan 9 text is UTF-8, so the δ glyph compares directly), and the LAST complete sample
// wins. ok is false when no such line is present (only "no sample" / "can't reach" lines).
func parseTimesync(data string) (clockSample, bool) {
	var last clockSample
	found := false
	for _, line := range strings.Split(data, "\n") {
		f := strings.Fields(line)
		var cs clockSample
		var haveOff, haveAvg, haveHz bool
		for i := 0; i+1 < len(f); i++ {
			v, err := strconv.ParseFloat(f[i+1], 64)
			if err != nil {
				continue
			}
			switch f[i] {
			case "δ":
				cs.offset, haveOff = v, true
			case "avgδ":
				cs.avgOffset, haveAvg = v, true
			case "hz":
				cs.freq, haveHz = v, true
			}
		}
		if haveOff && haveAvg && haveHz {
			last, found = cs, true
		}
	}
	return last, found
}

// etherStats holds the counters from /net/etherN/stats (sys/src/9/port/netif.c),
// which are labeled "in: <pkts>", "out: <pkts>", "link: <0|1>", "mbps: <speed>",
// and a set of cumulative error counters. Plan 9 counts packets, not bytes.
type etherStats struct {
	inPkts, outPkts float64
	hasIn, hasOut   bool

	// cumulative hardware/driver error counters
	crcErrs, overflows, softOverflows, framingErrs, bufferErrs, outputErrs float64

	link    float64 // 0/1
	hasLink bool
	mbps    float64 // link speed
	hasMbps bool
}

// errTotal sums the six error counters.
func (s etherStats) errTotal() float64 {
	return s.crcErrs + s.overflows + s.softOverflows + s.framingErrs + s.bufferErrs + s.outputErrs
}

func parseEtherStats(data string) etherStats {
	var s etherStats
	for _, line := range strings.Split(data, "\n") {
		label, val, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		v, err := strconv.ParseFloat(strings.TrimSpace(val), 64)
		if err != nil {
			continue // e.g. the "addr:" line, or labels with no number
		}
		switch strings.TrimSpace(label) {
		case "in":
			s.inPkts, s.hasIn = v, true
		case "out":
			s.outPkts, s.hasOut = v, true
		case "crc errs":
			s.crcErrs = v
		case "overflows":
			s.overflows = v
		case "soft overflows":
			s.softOverflows = v
		case "framing errs":
			s.framingErrs = v
		case "buffer errs":
			s.bufferErrs = v
		case "output errs":
			s.outputErrs = v
		case "link":
			s.link, s.hasLink = v, true
		case "mbps":
			s.mbps, s.hasMbps = v, true
		}
	}
	return s
}

// connState returns the TCP connection state from a /net/tcp/<n>/status line, which
// the kernel formats with the state first (tcpstates[] in sys/src/9/ip/tcp.c):
// "Established qin 0 qout 0 …". Empty when the line carries no fields.
func connState(statusData string) string {
	f := strings.Fields(statusData)
	if len(f) == 0 {
		return ""
	}
	return f[0]
}

// countConnStates tallies TCP connection states (lower-cased) across a set of
// /net/tcp/<n>/status contents.
func countConnStates(statuses []string) map[string]int {
	counts := make(map[string]int)
	for _, s := range statuses {
		if st := connState(s); st != "" {
			counts[strings.ToLower(st)]++
		}
	}
	return counts
}

// parseNetStats parses a "Label: value" stats file (the shape of /net/<proto>/stats)
// into a label→value map. Lines whose value isn't a single number (e.g. ICMP's
// per-type "Echo: 5 3" rows) are skipped, leaving the scalar counters.
func parseNetStats(data string) map[string]float64 {
	out := make(map[string]float64)
	for _, line := range strings.Split(data, "\n") {
		label, val, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		v, err := strconv.ParseFloat(strings.TrimSpace(val), 64)
		if err != nil {
			continue
		}
		out[strings.TrimSpace(label)] = v
	}
	return out
}

// ipifcStatus is the slice of a /net/ipifc/<n>/status header line we use. The kernel
// writes a flat "key value …" list (sys/src/9/ip/ipifc.c): "device %s maxtu %d …
// pktin %lud pktout %lud errin %lud errout %lud", then per-address lines we ignore.
type ipifcStatus struct {
	device                       string
	mtu                          float64
	pktin, pktout, errin, errout float64
}

func parseIpifcStatus(data string) (ipifcStatus, bool) {
	line := data
	if i := strings.IndexByte(data, '\n'); i >= 0 {
		line = data[:i] // the header is the first line
	}
	f := strings.Fields(line)
	var s ipifcStatus
	num := func(x string) float64 { v, _ := strconv.ParseFloat(x, 64); return v }
	// Scan for each known key and take the following field, robust to extra keys.
	for i := 0; i+1 < len(f); i++ {
		switch f[i] {
		case "device":
			s.device = f[i+1]
		case "maxtu":
			s.mtu = num(f[i+1])
		case "pktin":
			s.pktin = num(f[i+1])
		case "pktout":
			s.pktout = num(f[i+1])
		case "errin":
			s.errin = num(f[i+1])
		case "errout":
			s.errout = num(f[i+1])
		}
	}
	return s, s.device != ""
}

// Plan 9 /proc/N/status is a fixed-width line (sys/src/9/port/devproc.c, Qstatus):
// the program name and user in KNAMELEN(28)-byte space-padded columns, the state in
// a 12-byte column, then nine right-justified 12-byte numbers (the six times,
// memory in kB, basepri, priority).
const (
	knamelen       = 28              // KNAMELEN, the name/user column width
	statusNumStart = 2*knamelen + 12 // the numeric columns begin after name, user, state
)

// procStatus is the slice of /proc/N/status we use.
type procStatus struct {
	name  string
	state string // scheduler state or psstate wait name, e.g. Running/Ready/Wakeme/Fault
	memKB float64
}

// parseProcStatus slices the text columns at their fixed widths (a program name or
// user may itself contain a space, which a field split would let shift every later
// value) and field-splits only the numeric remainder. Memory is the seventh number,
// a position every kernel lineage keeps (the nix one appends ntrap/nintr/nsyscall
// after priority, so counting numbers from the right is not portable).
func parseProcStatus(data string) (procStatus, bool) {
	if len(data) < statusNumStart {
		return procStatus{}, false
	}
	nums := strings.Fields(data[statusNumStart:])
	if len(nums) < 9 {
		return procStatus{}, false
	}
	mem, err := strconv.ParseFloat(nums[6], 64)
	if err != nil {
		return procStatus{}, false
	}
	return procStatus{
		name:  strings.TrimSpace(data[:knamelen]),
		state: strings.TrimSpace(data[2*knamelen : statusNumStart]),
		memKB: mem,
	}, true
}

// procShareCount returns how many procs share this proc's address space, read from the
// contents of /proc/N/segment. On Plan 9 the threads of a program are separate procs
// created with rfork(RFMEM) (a Go program is ~16 of them), so they share their writable
// Data/Bss segments and the kernel's reference count on those segments is the number of
// sharers. Every sharer's /proc/N/status reports the SAME memory, the shared
// Text+Data+Bss image (the private per-thread stack is excluded), so summing it by
// program over-counts an N-thread program N×. Dividing each by this count first counts
// the image once. Only Data/Bss are consulted: Text is shared via the image cache
// between *unrelated* procs running the same binary (its refcount would over-divide),
// and the Stack is private (ref 1). Each line is "type [R] base top ref" (devproc.c:
// %-6s %c %.8lux %.8lux %4ld). The refcount is the last field. Returns 1 (no division)
// when no Data/Bss line is found, the file is empty, or it cannot be read.
func procShareCount(data string) uint64 {
	n := uint64(1)
	for _, line := range strings.Split(data, "\n") {
		f := strings.Fields(line)
		if len(f) < 4 || (f[0] != "Data" && f[0] != "Bss") {
			continue
		}
		if ref, err := strconv.ParseUint(f[len(f)-1], 10, 64); err == nil && ref > n {
			n = ref
		}
	}
	return n
}

// procStateBucket maps a Plan 9 scheduler state to a coarse, bounded bucket for the
// system.proc.count{state:…} breakdown. The scheduler states are statename[] in
// sys/src/9/port/proc.c. Everything else (the psstate wait-channel names like Fault,
// Idle, Pageout) is a descheduled wait and reads as "sleeping".
func procStateBucket(state string) string {
	switch state {
	case "Running", "Scheding":
		return "running"
	case "Ready", "New":
		return "ready"
	case "Queueing", "QueueingR", "QueueingW":
		return "blocked" // waiting on a qlock
	case "Stopped":
		return "stopped"
	case "Moribund", "Dead":
		return "zombie"
	case "Broken":
		return "broken"
	default:
		return "sleeping"
	}
}

// procStateBuckets is the full set procStateBucket can return, in display order. The
// collector emits system.proc.count for every bucket (0 when absent) so the per-state
// series stay continuous.
var procStateBuckets = []string{"running", "ready", "sleeping", "blocked", "stopped", "zombie", "broken"}

// nameMem is one program's summed memory, used for the top-by-memory ranking.
type nameMem struct {
	name  string
	memKB float64
}

// topProcsByMem aggregates per-process memory by program name (Plan 9 runs many
// processes per program) and returns the top n by total memory, descending. Ties
// break by name so the result is deterministic.
func topProcsByMem(procs []procStatus, n int) []nameMem {
	byName := make(map[string]float64, len(procs))
	for _, p := range procs {
		byName[p.name] += p.memKB
	}
	out := make([]nameMem, 0, len(byName))
	for name, mem := range byName {
		out = append(out, nameMem{name, mem})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].memKB != out[j].memKB {
			return out[i].memKB > out[j].memKB
		}
		return out[i].name < out[j].name
	})
	if len(out) > n {
		out = out[:n]
	}
	return out
}

// parseVentiStorage is intentionally not here: venti is read over HTTP (not a
// /dev file) by the build-tag-neutral internal/venti package, so it can run and be
// tested on any OS.

func parseUint(s string) uint64 {
	v, _ := strconv.ParseUint(s, 10, 64)
	return v
}

// parseFraction splits a "used/total" field.
func parseFraction(s string) (used, total uint64) {
	a, b, ok := strings.Cut(s, "/")
	if !ok {
		return 0, 0
	}
	return parseUint(a), parseUint(b)
}
