package host

import (
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/0intro/dd-micro-agent/internal/metrics"
)

// collectors returns the Plan 9 sub-collectors. Everything is read from the
// kernel's text files under /dev, /net, and /proc (no syscalls, no x/sys). Disk
// usage is omitted here (Plan 9 has no statfs). Optional venti storage is reported
// separately over HTTP by internal/venti.
func collectors(_ *Collector) []subCollector {
	return []subCollector{
		&plan9CPU{},
		&plan9Cputemp{},
		&plan9Battery{},
		&plan9Mem{},
		&plan9Uptime{},
		&plan9Net{},
		&plan9Ifstats{},
		&plan9TCPConns{},
		&plan9NetProto{},
		&plan9NetTables{},
		&plan9Proc{},
		&plan9Clock{},
		&plan9SrvMnt{},
	}
}

// cpuTag is the per-CPU tag for the /dev/sysstat metrics.
func cpuTag(id int) string { return "cpu:" + strconv.Itoa(id) }

// cpu + load: /dev/sysstat, one read per collect

// plan9CPU reads /dev/sysstat once per collect and emits everything it carries:
// the core count, per-core idle % and interrupt % (tagged cpu:N, like the stock
// Agent, so the host value is the average over the tag), the load, and per-second
// rates of the six cumulative counter columns (context switches, interrupts,
// syscalls, faults, tlb faults, tlb purges), summed across cores and diffed
// against the previous read. Plan 9 exposes only idle % and interrupt %, no
// user/system. Like the other rate collectors, the first read only establishes
// the baseline for the rates.
type plan9CPU struct {
	prev   sysstatTotals
	prevTs time.Time
	has    bool
}

func (*plan9CPU) name() string { return "cpu" }

func (c *plan9CPU) collect(now time.Time) ([]metrics.Serie, error) {
	data, err := os.ReadFile("/dev/sysstat")
	if err != nil {
		return nil, err
	}
	rows := parseSysstatRows(string(data))
	if len(rows) == 0 {
		return nil, nil
	}
	t := sumSysstat(rows)
	out := []metrics.Serie{gauge("system.cpu.num_cores", now, float64(t.ncpu))}
	for _, r := range rows {
		out = append(out,
			gauge("system.cpu.idle", now, r.idlePct, cpuTag(r.id)),
			gauge("system.cpu.intr_pct", now, r.intrPct, cpuTag(r.id)),
		)
	}
	// load is an instantaneous EWMA (the column is per-mille). Plan 9 has no
	// 1/5/15 decomposition.
	load := t.load / 1000
	out = append(out,
		gauge("system.load.1", now, load),
		gauge("system.load.norm.1", now, load/float64(t.ncpu)),
	)
	if c.has {
		if dt := now.Sub(c.prevTs).Seconds(); dt > 0 {
			rate := func(name string, cur, prev float64) {
				if cur >= prev { // skip counter resets (reboot)
					out = append(out, gauge(name, now, (cur-prev)/dt))
				}
			}
			rate("system.cpu.context_switches", t.ctxt, c.prev.ctxt)
			rate("system.cpu.interrupts", t.intr, c.prev.intr)
			rate("system.cpu.syscalls", t.syscall, c.prev.syscall)
			rate("system.cpu.faults", t.fault, c.prev.fault)
			rate("system.cpu.tlb_faults", t.tlbFault, c.prev.tlbFault)
			rate("system.cpu.tlb_purges", t.tlbPurge, c.prev.tlbPurge)
		}
	}
	c.prev, c.prevTs, c.has = t, now, true
	return out, nil
}

// cpu temperature: /dev/cputemp, one gauge per thermal sensor (°C)

type plan9Cputemp struct{}

func (plan9Cputemp) name() string { return "cputemp" }

func (plan9Cputemp) collect(now time.Time) ([]metrics.Serie, error) {
	data, err := os.ReadFile("/dev/cputemp")
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil // no thermal-sensor file on this arch: not an error
		}
		return nil, err
	}
	// Per-sensor, tagged cpu:N like idle/intr_pct, so the host value is the average
	// over the cpu tag. An "unsupported" sentinel yields no temps and emits nothing.
	var out []metrics.Serie
	for i, t := range parseCputemp(string(data)) {
		out = append(out, gauge("system.cpu.temp", now, t, cpuTag(i)))
	}
	return out, nil
}

// battery: /mnt/apm/battery (or /dev/battery), charge percent

type plan9Battery struct{}

func (plan9Battery) name() string { return "battery" }

func (plan9Battery) collect(now time.Time) ([]metrics.Serie, error) {
	for _, path := range []string{"/mnt/apm/battery", "/dev/battery"} {
		data, err := os.ReadFile(path)
		if err != nil {
			continue // absent on most hosts. Try the next path
		}
		if pct, ok := parseBattery(string(data)); ok {
			return []metrics.Serie{gauge("system.battery.pct", now, pct)}, nil
		}
		return nil, nil // file present but no usable reading
	}
	return nil, nil // no battery on this host
}

// memory: /dev/swap, reported in MB

type plan9Mem struct{}

func (plan9Mem) name() string { return "memory" }

func (plan9Mem) collect(now time.Time) ([]metrics.Serie, error) {
	data, err := os.ReadFile("/dev/swap")
	if err != nil {
		return nil, err
	}
	s := parseSwap(string(data))
	if s.memBytes == 0 {
		return nil, nil
	}
	return swapSeries(s, swapCapacityBytes(), now), nil
}

// swapCapacityBytes returns the configured swap size in bytes, or 0 if no swap is
// attached. /dev/swap's "total" field can't be used for this: it's the kernel's
// boot-time slot ceiling (conf.nproc*80 pages, ~625 MB on a default kernel),
// reported whether or not a store is attached. swap(8) instead records the
// backing-store path in /env/swap before handing the fd to the kernel, so the
// file's presence is the "swap configured" signal and the partition/file it names
// is stat'd for the true capacity (which also fixes the under-report when the store
// is larger than the ceiling: the kernel only clamps the ceiling down, never up).
func swapCapacityBytes() uint64 {
	path, err := os.ReadFile("/env/swap")
	if err != nil {
		return 0 // /env/swap absent: no swap configured
	}
	p := strings.Trim(string(path), " \t\r\n\x00")
	if p == "" {
		return 0
	}
	fi, err := os.Stat(p)
	if err != nil {
		return 0 // path recorded but unstattable: report no capacity rather than guess
	}
	return uint64(fi.Size())
}

// uptime: /dev/time

type plan9Uptime struct{}

func (plan9Uptime) name() string { return "uptime" }

func (plan9Uptime) collect(now time.Time) ([]metrics.Serie, error) {
	data, err := os.ReadFile("/dev/time")
	if err != nil {
		return nil, err
	}
	up := parseUptime(string(data))
	if up <= 0 {
		return nil, nil
	}
	return []metrics.Serie{gauge("system.uptime", now, up)}, nil
}

// network: /net/etherN/stats packet + error counters, link, and speed

// plan9Net reports per-interface throughput, errors, link state, and speed. Plan 9
// counts packets, not bytes, so only packet/error rates are available. The first
// read only establishes the baseline for those rates.
type plan9Net struct {
	prev   map[string]etherStats
	prevTs time.Time
}

func (nc *plan9Net) name() string { return "network" }

func (nc *plan9Net) collect(now time.Time) ([]metrics.Serie, error) {
	ifaces, err := etherInterfaces()
	if err != nil {
		return nil, err
	}
	cur := make(map[string]etherStats, len(ifaces))
	for _, ifc := range ifaces {
		data, err := os.ReadFile(filepath.Join("/net", ifc, "stats"))
		if err != nil {
			continue
		}
		cur[ifc] = parseEtherStats(string(data))
	}

	var out []metrics.Serie
	// Link state and speed are instantaneous gauges (emit on every collect).
	for ifc, c := range cur {
		dev := "device:" + ifc
		if c.hasLink {
			out = append(out, gauge("system.net.iface.up", now, c.link, dev))
		}
		if c.hasMbps {
			out = append(out, gauge("system.net.iface.speed_mbps", now, c.mbps, dev))
		}
	}
	// Packet and error throughput are rates (diff two reads).
	if nc.prev != nil {
		if dt := now.Sub(nc.prevTs).Seconds(); dt > 0 {
			for ifc, c := range cur {
				p, ok := nc.prev[ifc]
				if !ok {
					continue
				}
				dev := "device:" + ifc
				if c.hasIn && p.hasIn && c.inPkts >= p.inPkts {
					out = append(out, gauge("system.net.packets_in.count", now, (c.inPkts-p.inPkts)/dt, dev))
				}
				if c.hasOut && p.hasOut && c.outPkts >= p.outPkts {
					out = append(out, gauge("system.net.packets_out.count", now, (c.outPkts-p.outPkts)/dt, dev))
				}
				if et, pe := c.errTotal(), p.errTotal(); et >= pe {
					out = append(out, gauge("system.net.errors.count", now, (et-pe)/dt, dev))
				}
			}
		}
	}
	nc.prev = cur
	nc.prevTs = now
	return out, nil
}

// etherInterfaces lists the ethernet interface directories under /net (ether0,
// ether1, …).
func etherInterfaces() ([]string, error) {
	entries, err := os.ReadDir("/net")
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "ether") {
			out = append(out, e.Name())
		}
	}
	return out, nil
}

// processes: /proc/*/status, count + total + top-N by memory

const procTopN = 20 // cap the per-process memory series to bound cardinality

type plan9Proc struct{}

func (plan9Proc) name() string { return "proc" }

func (plan9Proc) collect(now time.Time) ([]metrics.Serie, error) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, err
	}
	const kbPerMB = 1024.0 // /proc/N/status memory is in kB

	var procs []procStatus
	var totalKB float64
	byState := make(map[string]int)
	for _, e := range entries {
		if !allDigits(e.Name()) {
			continue // skip non-process entries
		}
		data, err := os.ReadFile(filepath.Join("/proc", e.Name(), "status"))
		if err != nil {
			continue // process exited mid-scan: expected, do not log
		}
		ps, ok := parseProcStatus(string(data))
		if !ok {
			continue
		}
		// A Plan 9 program is many rfork(RFMEM) procs sharing one address space, and each
		// reports the SAME status memory (the shared Text+Data+Bss image), so summing it
		// by program over-counts an N-thread program N×. Divide by the address-space share
		// count (the Data/Bss segment refcount in /proc/N/segment) so the shared image is
		// counted once. If segment is unreadable (proc exited mid-scan) the count is 1, so
		// the size is left as the kernel reported it.
		if seg, err := os.ReadFile(filepath.Join("/proc", e.Name(), "segment")); err == nil {
			ps.memKB /= float64(procShareCount(string(seg)))
		}
		procs = append(procs, ps)
		totalKB += ps.memKB
		byState[procStateBucket(ps.state)]++
	}

	// system.proc.count is tagged by state. The host total is the sum over the tag.
	// Every bucket is emitted (0 when absent) so the series stay continuous.
	out := []metrics.Serie{gauge("system.proc.memory.total", now, totalKB/kbPerMB)}
	for _, st := range procStateBuckets {
		out = append(out, gauge("system.proc.count", now, float64(byState[st]), "state:"+st))
	}
	for _, p := range topProcsByMem(procs, procTopN) {
		out = append(out, gauge("system.proc.memory", now, p.memKB/kbPerMB, "proc:"+p.name))
	}
	return out, nil
}

// allDigits reports whether s is a non-empty run of ASCII digits (a /proc PID dir).
func allDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// tcp connection states: /net/tcp/<n>/status, counted by state

// tcpConnStateNames is every state plan9TCPConns emits. Established is excluded (the
// kernel reports it authoritatively as CurrEstab, shipped as current_established by
// plan9NetProto), and so is Closed: Plan 9 preallocates the numbered /net/tcp conversation
// directories, so idle slots sit in the Closed state and counting them would report
// free slots, not closed connections. The rest are emitted on every collect (0 when
// absent) so the per-state series stay continuous.
var tcpConnStateNames = []string{
	"listen", "syn_sent", "syn_received",
	"finwait1", "finwait2", "close_wait", "closing", "last_ack", "time_wait",
}

type plan9TCPConns struct{}

func (plan9TCPConns) name() string { return "tcpconns" }

func (plan9TCPConns) collect(now time.Time) ([]metrics.Serie, error) {
	entries, err := os.ReadDir("/net/tcp")
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil // no TCP stack configured
		}
		return nil, err
	}
	var statuses []string
	for _, e := range entries {
		if !allDigits(e.Name()) {
			continue // skip clone, stats, … (only numbered conversations)
		}
		data, err := os.ReadFile(filepath.Join("/net/tcp", e.Name(), "status"))
		if err != nil {
			continue // connection closed mid-scan
		}
		statuses = append(statuses, string(data))
	}
	counts := countConnStates(statuses)
	out := make([]metrics.Serie, 0, len(tcpConnStateNames))
	for _, st := range tcpConnStateNames {
		out = append(out, gauge("system.net.tcp."+st, now, float64(counts[st])))
	}
	return out, nil
}

// per-protocol stats: /net/{ipifc,icmp,udp,tcp}/stats

// netProtoSources maps each protocol's "Label: value" stats file to its metric prefix
// and the kernel-label→suffix tables. IP-level stats live at /net/ipifc/stats (the ipifc
// pseudo-protocol owns them). labels are cumulative counters, shipped as per-second
// rates. gauges are instantaneous values, shipped as-is on every collect: TCP's
// CurrEstab (current_established matches the stock Agent, networkv2) and InLimbo
// (half-open SYN-received connections, a SYN-flood/backlog signal).
var netProtoSources = []struct {
	path, prefix string
	labels       map[string]string // cumulative counters -> rates
	gauges       map[string]string // instantaneous values -> gauges
}{
	{"/net/ipifc/stats", "system.net.ip.", map[string]string{
		"InReceives": "in_receives", "InHdrErrors": "in_header_errors", "InAddrErrors": "in_addr_errors",
		"ForwDatagrams": "forwarded_datagrams", "InUnknownProtos": "in_unknown_protos", "InDiscards": "in_discards",
		"InDelivers": "in_delivers", "OutRequests": "out_requests", "OutDiscards": "out_discards", "OutNoRoutes": "out_no_routes",
		"ReasmTimeout": "reassembly_timeouts", "ReasmReqds": "reassembly_requests", "ReasmOKs": "reassembly_oks", "ReasmFails": "reassembly_fails",
		"FragOKs": "fragmentation_oks", "FragFails": "fragmentation_fails", "FragCreates": "fragmentation_creates",
	}, nil},
	{"/net/icmp/stats", "system.net.icmp.", map[string]string{
		"InMsgs": "in_msgs", "InErrors": "in_errors", "OutMsgs": "out_msgs",
		"CsumErrs": "csum_errs", "LenErrs": "len_errs", "HlenErrs": "hlen_errs",
	}, nil},
	{"/net/udp/stats", "system.net.udp.", map[string]string{
		"InDatagrams": "in_datagrams", "NoPorts": "no_ports", "InErrors": "in_errors", "OutDatagrams": "out_datagrams",
	}, nil},
	{"/net/tcp/stats", "system.net.tcp.", map[string]string{
		"InSegs": "in_segs", "OutSegs": "out_segs",
		"RetransSegs": "retrans_segs", "RetransTimeouts": "retrans_timeouts",
		"EstabResets": "established_resets", "InErrs": "in_errors",
		"ActiveOpens": "active_opens", "PassiveOpens": "passive_opens", "OutRsts": "out_resets",
	}, map[string]string{
		"CurrEstab": "current_established", "InLimbo": "in_limbo",
	}},
}

type plan9NetProto struct {
	prev   map[string]float64 // metric name -> cumulative value
	prevTs time.Time
}

func (*plan9NetProto) name() string { return "netproto" }

func (c *plan9NetProto) collect(now time.Time) ([]metrics.Serie, error) {
	var out []metrics.Serie
	cur := make(map[string]float64)
	for _, src := range netProtoSources {
		data, err := os.ReadFile(src.path)
		if err != nil {
			continue // protocol not present. Skip silently
		}
		stats := parseNetStats(string(data))
		for label, suffix := range src.labels {
			if v, ok := stats[label]; ok {
				cur[src.prefix+suffix] = v
			}
		}
		for label, suffix := range src.gauges {
			if v, ok := stats[label]; ok {
				out = append(out, gauge(src.prefix+suffix, now, v))
			}
		}
	}
	if c.prev != nil {
		if dt := now.Sub(c.prevTs).Seconds(); dt > 0 {
			for name, v := range cur {
				if p, ok := c.prev[name]; ok && v >= p { // skip resets (reboot)
					out = append(out, gauge(name, now, (v-p)/dt))
				}
			}
		}
	}
	c.prev, c.prevTs = cur, now
	return out, nil
}

// interface MTU/packets + route/arp table sizes

type ifaceCounters struct{ pktin, pktout, errin, errout float64 }

type plan9NetTables struct {
	prev   map[string]ifaceCounters // device -> counters
	prevTs time.Time
}

func (*plan9NetTables) name() string { return "nettables" }

func (c *plan9NetTables) collect(now time.Time) ([]metrics.Serie, error) {
	var out []metrics.Serie

	// Per-interface MTU (gauge) + packet/error rates, from /net/ipifc/<n>/status.
	cur := make(map[string]ifaceCounters)
	if entries, err := os.ReadDir("/net/ipifc"); err == nil {
		for _, e := range entries {
			if !allDigits(e.Name()) {
				continue
			}
			data, err := os.ReadFile(filepath.Join("/net/ipifc", e.Name(), "status"))
			if err != nil {
				continue
			}
			s, ok := parseIpifcStatus(string(data))
			if !ok {
				continue
			}
			name := ifaceDevice(s.device)
			if name == "null" {
				continue // the null medium (loopback-ish) carries no real traffic. Skip the noise
			}
			dev := "device:" + name
			if s.mtu > 0 {
				out = append(out, gauge("system.net.iface.mtu", now, s.mtu, dev))
			}
			cur[s.device] = ifaceCounters{s.pktin, s.pktout, s.errin, s.errout}
		}
	}
	if c.prev != nil {
		if dt := now.Sub(c.prevTs).Seconds(); dt > 0 {
			for dev, cc := range cur {
				p, ok := c.prev[dev]
				if !ok {
					continue
				}
				tag := "device:" + ifaceDevice(dev)
				rate := func(name string, curv, prevv float64) {
					if curv >= prevv {
						out = append(out, gauge(name, now, (curv-prevv)/dt, tag))
					}
				}
				rate("system.net.iface.packets_in", cc.pktin, p.pktin)
				rate("system.net.iface.packets_out", cc.pktout, p.pktout)
				rate("system.net.iface.errors_in", cc.errin, p.errin)
				rate("system.net.iface.errors_out", cc.errout, p.errout)
			}
		}
	}
	c.prev, c.prevTs = cur, now

	// Routing- and ARP-table sizes (gauges).
	if n, ok := countLines("/net/iproute"); ok {
		out = append(out, gauge("system.net.iproute.count", now, float64(n)))
	}
	if n, ok := countLines("/net/arp"); ok {
		out = append(out, gauge("system.net.arp.entries", now, float64(n)))
	}
	return out, nil
}

// ifaceDevice reduces an ipifc device field (e.g. "/dev/ether0", "#l0") to a clean
// tag: the last path element, with a leading kernel device-name '#' stripped
// (Datadog renders a '#' in a tag value as '_', so "#l0" would show as "_l0").
func ifaceDevice(d string) string {
	d = strings.TrimRight(d, "/")
	if i := strings.LastIndexByte(d, '/'); i >= 0 {
		d = d[i+1:]
	}
	d = strings.TrimPrefix(d, "#")
	if d == "" {
		return "unknown"
	}
	return d
}

// countLines counts non-blank lines in a file, ok is false if it can't be read.
func countLines(path string) (int, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	n := 0
	for _, line := range strings.Split(string(data), "\n") {
		if strings.TrimSpace(line) != "" {
			n++
		}
	}
	return n, true
}

// per-NIC driver stats: /net/etherN/ifstats counters as rates + wireless signal

// plan9Ifstats reports the rich, driver-specific counters from /net/etherN/ifstats (the
// basic packet/error counts live in /net/etherN/stats, handled by plan9Net). The field set
// varies by NIC: physical drivers (ether82557, ether8169) dump cumulative error/collision
// counters, while the virtio-net of a VM exposes only per-queue lines that carry no value
// and yield nothing here. All captured fields are shipped as per-second rates tagged
// device:etherN. The first read only establishes the baseline. A wireless interface's
// leading "Signal:" line is instantaneous and ships as the system.net.wifi.signal
// gauge (dBm) on every collect.
type plan9Ifstats struct {
	prev   map[string]map[string]float64 // iface -> label -> cumulative value
	prevTs time.Time
}

func (*plan9Ifstats) name() string { return "ifstats" }

func (c *plan9Ifstats) collect(now time.Time) ([]metrics.Serie, error) {
	ifaces, err := etherInterfaces()
	if err != nil {
		return nil, nil // no /net: plan9Net already surfaces this
	}
	var out []metrics.Serie
	cur := make(map[string]map[string]float64, len(ifaces))
	for _, ifc := range ifaces {
		data, err := os.ReadFile(filepath.Join("/net", ifc, "ifstats"))
		if err != nil {
			continue
		}
		s := string(data)
		if sig, ok := parseSignal(s); ok {
			out = append(out, gauge("system.net.wifi.signal", now, sig, "device:"+ifc))
		}
		if m := parseIfstats(s); len(m) > 0 {
			cur[ifc] = m
		}
	}

	if c.prev != nil {
		if dt := now.Sub(c.prevTs).Seconds(); dt > 0 {
			for ifc, m := range cur {
				p, ok := c.prev[ifc]
				if !ok {
					continue
				}
				dev := "device:" + ifc
				for label, v := range m {
					if pv, ok := p[label]; ok && v >= pv { // skip new labels and counter resets
						out = append(out, gauge("system.net.iface."+ifstatMetric(label), now, (v-pv)/dt, dev))
					}
				}
			}
		}
	}
	c.prev, c.prevTs = cur, now
	return out, nil
}

// clock discipline: /sys/log/timesync, last sample (offset ns, frequency Hz)

// plan9Clock reports the host clock's offset and disciplined frequency from aux/timesync,
// which logs them to /sys/log/timesync (the only source of computed drift: no kernel file
// has it). Best-effort: emits nothing when timesync isn't running or hasn't logged a
// sample. It reads only the file's tail so a long-running log isn't reloaded each flush.
type plan9Clock struct{}

func (plan9Clock) name() string { return "clock" }

func (plan9Clock) collect(now time.Time) ([]metrics.Serie, error) {
	data, err := readTail("/sys/log/timesync", 4096)
	if err != nil {
		return nil, nil // timesync not running / log absent: best-effort
	}
	s, ok := parseTimesync(data)
	if !ok {
		return nil, nil
	}
	return []metrics.Serie{
		gauge("system.clock.offset", now, s.offset),
		gauge("system.clock.offset_avg", now, s.avgOffset),
		gauge("system.clock.frequency", now, s.freq),
	}, nil
}

// service/mount counts: number of entries in /srv and /mnt

type plan9SrvMnt struct{}

func (plan9SrvMnt) name() string { return "srvmnt" }

func (plan9SrvMnt) collect(now time.Time) ([]metrics.Serie, error) {
	var out []metrics.Serie
	if n, ok := countDirEntries("/srv"); ok {
		out = append(out, gauge("system.srv.count", now, float64(n)))
	}
	if n, ok := countDirEntries("/mnt"); ok {
		out = append(out, gauge("system.mnt.count", now, float64(n)))
	}
	return out, nil
}

// countDirEntries counts entries in a directory, ok is false if it can't be read.
func countDirEntries(path string) (int, bool) {
	entries, err := os.ReadDir(path)
	if err != nil {
		return 0, false
	}
	return len(entries), true
}

// readTail returns up to the last n bytes of a file (the whole file if smaller), for
// cheaply reading the most recent lines of a growing log without loading all of it. The
// first line may be partial. Callers parse line-by-line and tolerate a truncated head.
func readTail(path string, n int64) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return "", err
	}
	off := int64(0)
	if fi.Size() > n {
		off = fi.Size() - n
	}
	buf := make([]byte, fi.Size()-off)
	nr, err := f.ReadAt(buf, off)
	if err != nil && err != io.EOF {
		return "", err
	}
	// On EOF (the file shrank between Stat and ReadAt) only nr bytes are valid.
	return string(buf[:nr]), nil
}
