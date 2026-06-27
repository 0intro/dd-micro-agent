package main

import (
	"strings"
	"testing"
)

// record constructors for golden recordings.
func sgauge(name string, v float64, tags ...string) record {
	return record{Kind: "series", Metric: name, Type: "gauge", Value: v, Interval: 15, Tags: tags}
}

func srate(name string, v float64, interval int64, tags ...string) record {
	return record{Kind: "series", Metric: name, Type: "rate", Value: v, Interval: interval, Tags: tags}
}

func logr(msg, source, service, status, ddtags string) record {
	return record{Kind: "log", Message: msg, Source: source, Service: service, Status: status, Ddtags: ddtags}
}

func metaRec(platform string) record {
	return record{Kind: "meta",
		Attrs: map[string]string{"platform": platform, "hostname": "parity-host", "os": "linux"},
		Nums:  map[string]float64{"cpu_cores": 4, "mem_total": 16e9},
		Tags:  []string{"test:parity"}}
}

func invHostRec() record {
	return record{Kind: "inv_host",
		Attrs: map[string]string{"os": "linux", "kernel_name": "Linux", "cpu_architecture": "x86_64"},
		Nums:  map[string]float64{"cpu_cores": 4, "cpu_logical": 8, "memory_total_kb": 16e6}}
}

func procRec(encoding, count int, names ...string) record {
	return record{Kind: "process", ProcType: typeCollectorProc, ProcEncoding: encoding, ProcCount: count, ProcNames: names}
}

func profRec(via, addTags, family string, attach ...string) record {
	return record{Kind: "profile", ProfVia: via, ProfAPIKey: true, ProfAddTags: addTags, ProfFamily: family, ProfAttach: attach}
}

// matched returns a pair of recordings that should be at parity across every
// signal, shaped like the real cross-agent case. Our agent appends the host tag
// test:parity to every serie and log on the wire, the stock agent sends them
// untagged (its host tags live in the metadata host-tags, which both carry) and
// tags logs with the file source instead. Stock also drops some of the histogram
// samples (DogStatsD is lossy), reads uptime a few seconds later, emits an extra
// host metric we don't, and posts a plain decodable process list, all allowed.
func matched() (ours, stock []record) {
	base := func(uptime float64, ddtags string, mtags ...string) []record {
		return []record{
			sgauge(parityPrefix+"gauge", 42, mtags...),
			sgauge(parityPrefix+"users", 5, mtags...),
			sgauge(parityPrefix+"latency.avg", 50, mtags...), // histogram aggregate, loss-invariant
			{Kind: "check", Check: parityPrefix + "check", CheckStatus: 0},
			{Kind: "event", Title: "dsdsample up"},
			logr("hello parity", "parity", "parity", "info", ddtags),
			sgauge("system.mem.total", 16000, mtags...),
			sgauge("system.uptime", uptime, mtags...),
			metaRec("linux"), // host-tags test:parity on both (the metadata, not the series)
			invHostRec(),
			{Kind: "inv_agent", Attrs: map[string]string{"agent_version": "1", "flavor": "agent"}},
			procRec(0, 50, "init", "agent", "cs"),
		}
	}
	// Both forward a profile through their proxy. The injected host tag and
	// attachment set match, only agent_version differs (each proxy stamps its own).
	// Ours gets all the histogram samples (latency.count 75), stock drops some (45).
	ours = append(base(1000, "test:parity", "test:parity"),
		srate(parityPrefix+"requests", 1, 15, "test:parity"),
		srate(parityPrefix+"requests", 1, 15, "test:parity"),
		srate(parityPrefix+"latency.count", 5, 15, "test:parity"),
		profRec("trace-agent 0.1.0", "host:parity-host,default_env:none,agent_version:0.1.0", "go", "cpu.pprof", "heap.pprof"),
	)
	stock = append(base(1007, "filename:app.log,dirname:/var/log/parity"),
		srate(parityPrefix+"requests", 2, 15),
		srate(parityPrefix+"latency.count", 3, 15), // 45, not 75: lossy, but present
		sgauge("system.cpu.user", 3.2),             // a metric we don't emit (allowed)
		profRec("trace-agent 7.66.1", "host:parity-host,default_env:none,agent_version:7.66.1", "go", "cpu.pprof", "heap.pprof", "goroutines.pprof"),
	)
	return ours, stock
}

func TestCompareMatching(t *testing.T) {
	report, pass := compare(matched())
	if !pass {
		t.Fatalf("expected parity, got FAIL:\n%s", report)
	}
	for _, want := range []string{
		"rate " + parityPrefix + "requests{} present on both",      // counter, presence only
		"rate " + parityPrefix + "latency.count{} present on both", // lossy histogram count, presence only
		parityPrefix + "gauge{} = 42.0000",                         // gauge, exact, host tag stripped
		parityPrefix + "latency.avg{} = 50.0000",                   // histogram aggregate, loss-invariant, exact
		"every system.* metric we emit is also emitted by the stock agent",
		`log "hello parity"`,
		"systemStats.platform = \"linux\"",
		"inventory_agent sent by both",
		"both POST a MessageV3 CollectorProc",
		"process counts comparable: ours=50 stock=50",
		"both forward a multipart upload to /api/v2/profile",
		"both set Via: trace-agent",
		"both inject DD-API-KEY",
		"both set X-Datadog-Additional-Tags host:parity-host",
		"attachment sets overlap: cpu.pprof, heap.pprof",
	} {
		if !strings.Contains(report, want) {
			t.Errorf("report missing %q\n%s", want, report)
		}
	}
}

// Counters are compared by presence, not value, because DogStatsD loses different
// UDP packets at each agent. A diverging run total must NOT fail (the exact
// counter-to-rate math is unit-tested in internal/metrics on lossless input). A
// counter MISSING from one agent must fail.
func TestCompareRateValueLossTolerant(t *testing.T) {
	ours, stock := matched()
	stock = replaceRate(stock, parityPrefix+"requests", 3) // 45, not 30: a value divergence
	report, pass := compare(ours, stock)
	if !pass {
		t.Fatalf("a diverging rate value should pass (counters compared by presence):\n%s", report)
	}
	if !strings.Contains(report, "rate "+parityPrefix+"requests{} present on both") {
		t.Errorf("report should note the rate present on both:\n%s", report)
	}
}

func TestCompareRateMissing(t *testing.T) {
	ours, stock := matched()
	var trimmed []record // drop the requests rate from stock
	for _, r := range stock {
		if r.Kind == "series" && r.Metric == parityPrefix+"requests" {
			continue
		}
		trimmed = append(trimmed, r)
	}
	report, pass := compare(ours, trimmed)
	if pass {
		t.Fatalf("expected FAIL when stock omits a rate ours sends:\n%s", report)
	}
	if !strings.Contains(report, "rate "+parityPrefix+"requests{}: stock did not send it") {
		t.Errorf("report should flag the missing rate:\n%s", report)
	}
}

func TestCompareLogMismatch(t *testing.T) {
	ours, stock := matched()
	ours = append(ours, logr("only ours", "parity", "parity", "info", "test:parity"))
	report, pass := compare(ours, stock)
	if pass {
		t.Fatalf("expected FAIL on a log only ours shipped:\n%s", report)
	}
	if !strings.Contains(report, "ours shipped it, stock did not") {
		t.Errorf("report should flag the ours-only log:\n%s", report)
	}
}

func TestCompareHostSubsetViolation(t *testing.T) {
	ours, stock := matched()
	ours = append(ours, sgauge("system.made.up", 1, "test:parity")) // we emit, stock doesn't
	report, pass := compare(ours, stock)
	if pass {
		t.Fatalf("expected FAIL when we emit a metric stock lacks:\n%s", report)
	}
	if !strings.Contains(report, "system.made.up") {
		t.Errorf("report should name the ours-only metric:\n%s", report)
	}
}

func TestCompareMetadataMismatch(t *testing.T) {
	ours, stock := matched()
	for i, r := range stock {
		if r.Kind == "meta" {
			stock[i] = metaRec("windows") // platform must match across agents
		}
	}
	report, pass := compare(ours, stock)
	if pass {
		t.Fatalf("expected FAIL on diverging systemStats.platform:\n%s", report)
	}
	if !strings.Contains(report, "systemStats.platform: ours=\"linux\" stock=\"windows\"") {
		t.Errorf("report should flag the platform divergence:\n%s", report)
	}
}

func TestCompareInventoryMissing(t *testing.T) {
	ours, stock := matched()
	var trimmed []record // drop the stock inventory_host
	for _, r := range stock {
		if r.Kind != "inv_host" {
			trimmed = append(trimmed, r)
		}
	}
	report, pass := compare(ours, trimmed)
	if pass {
		t.Fatalf("expected FAIL when stock sends no inventory_host:\n%s", report)
	}
	if !strings.Contains(report, "inventory_host not sent by both") {
		t.Errorf("report should flag missing inventory_host:\n%s", report)
	}
}

// TestCompareProcessZstdEnvelope is the real cross-agent case: the stock agent's
// process body is zstd (encoding != 0, undecodable). Envelope/type parity still
// holds, so the run passes with a note that the content isn't byte-comparable.
func TestCompareProcessZstdEnvelope(t *testing.T) {
	ours, stock := matched()
	for i, r := range stock {
		if r.Kind == "process" {
			stock[i] = procRec(2, 0) // zstd-encoded -> not decoded
		}
	}
	report, pass := compare(ours, stock)
	if !pass {
		t.Fatalf("envelope parity should pass despite an opaque stock body:\n%s", report)
	}
	if !strings.Contains(report, "opaque") {
		t.Errorf("report should note the opaque stock process body:\n%s", report)
	}
}

func TestCompareProcessNotSent(t *testing.T) {
	ours, stock := matched()
	var trimmed []record
	for _, r := range stock {
		if r.Kind != "process" {
			trimmed = append(trimmed, r)
		}
	}
	if _, pass := compare(ours, trimmed); pass {
		t.Fatal("expected FAIL when the stock agent sends no CollectorProc")
	}
}

// On darwin the stock Agent ships no process-agent, so a missing stock CollectorProc is
// expected: compareForPlatform("darwin") passes (with a note) where the default Linux
// comparison fails (TestCompareProcessNotSent), and it still reports our own count.
func TestCompareDarwinSkipsProcesses(t *testing.T) {
	ours, stock := matched()
	var noStockProc []record
	for _, r := range stock {
		if r.Kind != "process" {
			noStockProc = append(noStockProc, r)
		}
	}
	if _, pass := compareForPlatform(ours, noStockProc, ""); pass {
		t.Fatal("the linux comparison should fail when stock sends no CollectorProc")
	}
	report, pass := compareForPlatform(ours, noStockProc, "darwin")
	if !pass {
		t.Fatalf("darwin should pass without a stock CollectorProc:\n%s", report)
	}
	if !strings.Contains(report, "stock macOS sends no CollectorProc") {
		t.Errorf("report should note the skipped process tier:\n%s", report)
	}
	if !strings.Contains(report, "our process list still decodes: 50 processes") {
		t.Errorf("report should still report our own process count:\n%s", report)
	}
}

func TestCompareProfile(t *testing.T) {
	// only ours forwards a profile
	ours, stock := matched()
	var noProf []record
	for _, r := range stock {
		if r.Kind != "profile" {
			noProf = append(noProf, r)
		}
	}
	if report, pass := compare(ours, noProf); pass {
		t.Fatalf("expected FAIL when only ours forwards a profile:\n%s", report)
	}

	// neither forwards a profile (profiler not driven): a note, not a failure.
	var ourNoProf []record
	for _, r := range ours {
		if r.Kind != "profile" {
			ourNoProf = append(ourNoProf, r)
		}
	}
	if report, pass := compare(ourNoProf, noProf); !pass {
		t.Fatalf("neither forwarding a profile should pass with a note:\n%s", report)
	}

	// attachment sets disjoint
	ours, stock = matched()
	for i, r := range stock {
		if r.Kind == "profile" {
			stock[i] = profRec(r.ProfVia, r.ProfAddTags, "go", "mutex.pprof")
		}
	}
	report, pass := compare(ours, stock)
	if pass {
		t.Fatalf("expected FAIL when attachment sets are disjoint:\n%s", report)
	}
	if !strings.Contains(report, "attachment sets disjoint") {
		t.Errorf("report should flag disjoint attachments:\n%s", report)
	}
}

func TestDogstatsdValuesNormalizesRate(t *testing.T) {
	recs := []record{
		srate(parityPrefix+"requests", 1, 15, "test:parity"),
		srate(parityPrefix+"requests", 1, 15, "test:parity"),
		sgauge(parityPrefix+"gauge", 42, "test:parity"),
		sgauge("system.mem.total", 16000), // not a parity metric -> ignored
	}
	gauge, rate := dogstatsdValues(recs)
	// the host tag test:parity is stripped, so the key is {}, not {test:parity}
	if v := rate[parityPrefix+"requests{}"]; v != 30 {
		t.Errorf("counter total = %v, want 30 (1*15 + 1*15)", v)
	}
	if v := gauge[parityPrefix+"gauge{}"]; v != 42 {
		t.Errorf("gauge = %v, want 42", v)
	}
}

func TestSameTagsIgnoresOrderAndDupes(t *testing.T) {
	if !sameTags("a:1,b:2", "b:2,a:1") {
		t.Error("tag order should not matter")
	}
	if !sameTags("a:1,a:1,b:2", "a:1,b:2") {
		t.Error("duplicate tags should not matter")
	}
	if sameTags("a:1", "a:1,c:3") {
		t.Error("an extra tag should be detected")
	}
}

func TestDecodeCollectorProcRoundTrip(t *testing.T) {
	// Hand-build a minimal CollectorProc: two Process messages (field 3), each with
	// a Command (field 4) carrying a Comm (field 9). Proves the protobuf walker.
	proc := func(comm string) []byte {
		commBody := append([]byte{9<<3 | 2, byte(len(comm))}, comm...)         // Command{Comm} (field 9)
		cmdField := append([]byte{4<<3 | 2, byte(len(commBody))}, commBody...) // Process{Command} (field 4)
		return append([]byte{3<<3 | 2, byte(len(cmdField))}, cmdField...)      // CollectorProc{Process} (field 3)
	}
	body := append(proc("init"), proc("agent")...)
	count, names := decodeCollectorProc(body)
	if count != 2 {
		t.Fatalf("count = %d, want 2", count)
	}
	if strings.Join(names, ",") != "init,agent" {
		t.Errorf("names = %v, want [init agent]", names)
	}
}

// replaceRate rebuilds recs with a single rate record for name at the given rate.
func replaceRate(recs []record, name string, rate float64) []record {
	var out []record
	for _, r := range recs {
		if r.Kind == "series" && r.Metric == name {
			continue
		}
		out = append(out, r)
	}
	return append(out, srate(name, rate, 15, "test:parity"))
}
