package main

import (
	"fmt"
	"math"
	"sort"
	"strings"
)

// parityPrefix scopes the DogStatsD metrics the sample (e2e/dsdsample) emits, so
// Tier-1 comparison looks only at the workload we drove identically into both.
const parityPrefix = "microagent.parity."

// hostTag is the agent-level tag the parity test configures (datadog.yaml tags:).
// Our agent appends config.Tags to every serie and log on the wire (the curated
// Plan 9 dashboards key on a real platform:plan9 host tag), while the stock agent
// keeps host tags in the host metadata and lets the backend join them at query time.
// The two are backend-equivalent, so the comparison strips this tag before diffing
// values and verifies it separately on the v5 host-tags payload. Only this exact tag
// is stripped, so any other tag divergence still fails.
const hostTag = "test:parity"

// stripHostTag drops the configured host tag from a tag list.
func stripHostTag(tags []string) []string {
	var out []string
	for _, t := range tags {
		if t != hostTag {
			out = append(out, t)
		}
	}
	return out
}

// stableHostMetrics are the host gauges whose value should match across agents.
// Most host metrics fluctuate between the two agents' sample instants. These few
// don't (total RAM is fixed, uptime is monotonic and read seconds apart).
var stableHostMetrics = []hostCheck{
	{metric: "system.mem.total", relTol: 0.02}, // total RAM (MB), fixed
	{metric: "system.uptime", absTol: 60},      // seconds, read moments apart
}

type hostCheck struct {
	metric string
	relTol float64 // fraction of the stock value, used when > 0
	absTol float64 // absolute, used when relTol == 0
}

// compare diffs two recordings with the full (Linux) expectations. compare_test.go
// exercises this default; the CLI uses compareForPlatform for the per-OS runs.
func compare(ours, stock []record) (string, bool) {
	return compareForPlatform(ours, stock, "")
}

// compareForPlatform diffs two recordings (ours vs the stock agent) and returns a human
// report plus the overall pass/fail. platform tunes OS-specific expectations: "darwin"
// skips the Live Processes tier, since the stock macOS Agent ships no process-agent (Live
// Processes is Linux/Windows only). It is pure, so compare_test.go exercises it.
func compareForPlatform(ours, stock []record, platform string) (string, bool) {
	var b strings.Builder
	pass := true
	ok := func(f string, a ...any) { fmt.Fprintf(&b, "  ok    "+f+"\n", a...) }
	note := func(f string, a ...any) { fmt.Fprintf(&b, "  ..    "+f+"\n", a...) }
	fail := func(f string, a ...any) { pass = false; fmt.Fprintf(&b, "  FAIL  "+f+"\n", a...) }

	// Tier 1: DogStatsD metrics. Identical input yields identical output for the
	// loss-invariant values (gauges, histogram aggregates, set count), compared
	// exactly. Counters and histogram .count scale with how many UDP packets each
	// agent received, and DogStatsD is lossy by design, so those are verified by name
	// and rate type only (their exact value is unit-tested on lossless input).
	b.WriteString("DogStatsD metrics (gauges/aggregates exact, counters present):\n")
	og, orate := dogstatsdValues(ours)
	sg, srate := dogstatsdValues(stock)
	for _, key := range unionKeys(og, sg) {
		ov, ook := og[key]
		sv, sok := sg[key]
		switch {
		case !ook:
			fail("%s: ours did not send it (stock=%.4f)", key, sv)
		case !sok:
			fail("%s: stock did not send it (ours=%.4f)", key, ov)
		case approxEqual(ov, sv):
			ok("%s = %.4f", key, ov)
		default:
			fail("%s: ours=%.6f stock=%.6f", key, ov, sv)
		}
	}
	for _, key := range unionKeys(orate, srate) {
		_, ook := orate[key]
		_, sok := srate[key]
		switch {
		case ook && sok:
			ok("rate %s present on both (value is loss-sensitive, not compared)", key)
		case ook:
			fail("rate %s: stock did not send it", key)
		default:
			fail("rate %s: ours did not send it", key)
		}
	}
	checkPresence(ok, fail, "service check "+parityPrefix+"check",
		hasCheck(ours, parityPrefix+"check"), hasCheck(stock, parityPrefix+"check"))
	checkPresence(ok, fail, "event", hasEvent(ours), hasEvent(stock))

	// Tier 1: logs (same file tailed, so the shipped content must match). The log
	// identity is the message plus source/service/status. The ddtags differ by
	// design (we attach the host tags config.Tags, the stock agent attaches the file
	// source tags filename/dirname), so that is a note, not a failure.
	b.WriteString("Logs (exact content):\n")
	ol, sl := logsByMessage(ours), logsByMessage(stock)
	for msg, o := range ol {
		s, found := sl[msg]
		switch {
		case !found:
			fail("log %q: ours shipped it, stock did not", trunc(msg))
		case o.Source != s.Source || o.Service != s.Service || o.Status != s.Status:
			fail("log %q: ours=[%s/%s/%s] stock=[%s/%s/%s] (source/service/status)",
				trunc(msg), o.Source, o.Service, o.Status, s.Source, s.Service, s.Status)
		default:
			ok("log %q (source/service/status match)", trunc(msg))
			if !sameTags(o.Ddtags, s.Ddtags) {
				note("log %q ddtags differ by design: ours=%q stock=%q", trunc(msg), o.Ddtags, s.Ddtags)
			}
		}
	}
	for msg := range sl {
		if _, found := ol[msg]; !found {
			fail("log %q: stock shipped it, ours did not", trunc(msg))
		}
	}

	// Tier 2: host metrics (structural parity + a tolerance on stable values)
	b.WriteString("Host metrics (structural parity + tolerance):\n")
	oh, sh := hostTypes(ours), hostTypes(stock)
	var oursOnly []string
	for name, otype := range oh {
		stype, shared := sh[name]
		if !shared {
			oursOnly = append(oursOnly, name)
		} else if otype != stype {
			fail("%s: type ours=%q stock=%q", name, otype, stype)
		}
	}
	if len(oursOnly) == 0 {
		ok("every system.* metric we emit is also emitted by the stock agent")
	} else {
		sort.Strings(oursOnly)
		fail("%d metric(s) we emit are absent from the stock agent: %s",
			len(oursOnly), strings.Join(oursOnly, ", "))
	}
	note("stock emits %d system.* metric(s) we don't (expected, we are a subset)", countMissing(sh, oh))

	ohv, shv := hostValues(ours), hostValues(stock)
	for _, c := range stableHostMetrics {
		ov, ook := ohv[c.metric]
		sv, sok := shv[c.metric]
		tol := c.absTol
		if c.relTol > 0 {
			tol = c.relTol * math.Abs(sv)
		}
		switch {
		case !ook || !sok:
			note("%s: not comparable (ours=%v stock=%v)", c.metric, ook, sok)
		case math.Abs(ov-sv) <= tol:
			ok("%s: ours=%.2f stock=%.2f (within %.2f)", c.metric, ov, sv, tol)
		default:
			fail("%s: ours=%.2f stock=%.2f (exceeds %.2f)", c.metric, ov, sv, tol)
		}
	}

	// Host metadata (v5 /intake/), structural + host facts
	b.WriteString("Host metadata (v5 /intake/):\n")
	if om, sm, both := pair(ours, stock, "meta"); !both {
		fail("v5 host metadata not sent by both (ours=%v stock=%v)", has(ours, "meta"), has(stock, "meta"))
	} else {
		compareAttr(ok, fail, "systemStats.platform", om.Attrs["platform"], sm.Attrs["platform"])
		compareAttr(ok, fail, "meta.hostname", om.Attrs["hostname"], sm.Attrs["hostname"])
		if hasTag(om.Tags, hostTag) && hasTag(sm.Tags, hostTag) {
			ok("host-tags include %s on both", hostTag)
		} else {
			fail("host-tags missing %s (ours=%v stock=%v)", hostTag, om.Tags, sm.Tags)
		}
		// The v5 gohai cpu/memory facts use a legacy encoding (cpu_cores is per-socket,
		// not logical, and memory is a string), so cross-agent equality there is not
		// meaningful. The authoritative cpu_cores and memory_total_kb parity is checked
		// on the modern inventory below.
		note("gohai cpu/memory checked on the modern inventory (v5 uses a legacy encoding)")
		note("os field: ours=%q stock=%q", om.Attrs["os"], sm.Attrs["os"])
	}

	// Modern inventory (/api/v1/metadata)
	b.WriteString("Inventory (/api/v1/metadata):\n")
	if om, sm, both := pair(ours, stock, "inv_host"); !both {
		fail("inventory_host not sent by both (ours=%v stock=%v)", has(ours, "inv_host"), has(stock, "inv_host"))
	} else {
		compareAttr(ok, fail, "inventory os", om.Attrs["os"], sm.Attrs["os"])
		compareAttr(ok, fail, "inventory kernel_name", om.Attrs["kernel_name"], sm.Attrs["kernel_name"])
		compareAttr(ok, fail, "inventory cpu_architecture", om.Attrs["cpu_architecture"], sm.Attrs["cpu_architecture"])
		compareNum(ok, fail, "inventory cpu_cores", om.Nums["cpu_cores"], sm.Nums["cpu_cores"], 0)
		compareNum(ok, fail, "inventory memory_total_kb", om.Nums["memory_total_kb"], sm.Nums["memory_total_kb"], 0.02)
	}
	if has(ours, "inv_agent") && has(stock, "inv_agent") {
		ok("inventory_agent sent by both (agent_version present)")
	} else {
		fail("inventory_agent not sent by both (ours=%v stock=%v)", has(ours, "inv_agent"), has(stock, "inv_agent"))
	}

	// Live Processes (/api/v1/collector)
	b.WriteString("Live Processes (/api/v1/collector):\n")
	op, sp := processSummary(ours), processSummary(stock)
	switch {
	case platform == "darwin" && !sp.sent:
		// The stock macOS Agent ships no process-agent, so there is no CollectorProc to
		// diff against (Live Processes is Linux/Windows only). Report ours for information.
		note("stock macOS sends no CollectorProc (process-agent is Linux/Windows only); not compared")
		if op.decoded && op.count > 0 {
			ok("our process list still decodes: %d processes (e.g. %s)", op.count, strings.Join(sample(op.names, 5), ", "))
		}
	case !op.sent || !sp.sent:
		fail("CollectorProc not sent by both (ours=%v stock=%v)", op.sent, sp.sent)
	default:
		ok("both POST a MessageV3 CollectorProc (type %d) to /api/v1/collector", typeCollectorProc)
		// Our payload is plain protobuf and decodable. The stock body is zstd
		// (charter excludes zstd), so its contents are opaque. The wire FORMAT is
		// what's verified to match. Where both decode (e.g. the self-parity smoke),
		// the process lists are compared too.
		if op.decoded && op.count == 0 {
			fail("our CollectorProc decoded to 0 processes")
		} else if op.decoded {
			ok("our process list decodes: %d processes (e.g. %s)", op.count, strings.Join(sample(op.names, 5), ", "))
		}
		switch {
		case !sp.decoded:
			note("stock process body is encoding=%d (zstd), opaque. Type/framing verified, content not byte-comparable", sp.encoding)
		case op.decoded && within(float64(op.count), float64(sp.count), 0.25):
			ok("process counts comparable: ours=%d stock=%d", op.count, sp.count)
		case op.decoded:
			fail("process counts differ: ours=%d stock=%d", op.count, sp.count)
		}
	}

	// Profiling (/api/v2/profile). The profiler is identical on both agents, so the
	// proxy's own contribution is what we compare: that both forward an upload, both
	// inject the Via/DD-API-KEY/additional-tags headers, and the attachment sets
	// overlap. The pprof bytes are not byte-comparable (separate process runs).
	b.WriteString("Profiling (/api/v2/profile):\n")
	op2, oprof := firstOfKind(ours, "profile")
	sp2, sprof := firstOfKind(stock, "profile")
	switch {
	case !oprof && !sprof:
		note("no profile forwarded by either agent (profiler not driven in this run)")
	case !oprof || !sprof:
		fail("profile forwarded by only one agent (ours=%v stock=%v)", oprof, sprof)
	default:
		ok("both forward a multipart upload to /api/v2/profile")
		if strings.HasPrefix(op2.ProfVia, "trace-agent ") && strings.HasPrefix(sp2.ProfVia, "trace-agent ") {
			ok("both set Via: trace-agent")
		} else {
			fail("Via header ours=%q stock=%q", op2.ProfVia, sp2.ProfVia)
		}
		if op2.ProfAPIKey && sp2.ProfAPIKey {
			ok("both inject DD-API-KEY")
		} else {
			fail("DD-API-KEY presence ours=%v stock=%v", op2.ProfAPIKey, sp2.ProfAPIKey)
		}
		oh, sh := tagValue(op2.ProfAddTags, "host"), tagValue(sp2.ProfAddTags, "host")
		if oh != "" && oh == sh {
			ok("both set X-Datadog-Additional-Tags host:%s", oh)
		} else {
			fail("additional-tags host ours=%q stock=%q", op2.ProfAddTags, sp2.ProfAddTags)
		}
		if shared := intersect(op2.ProfAttach, sp2.ProfAttach); len(shared) > 0 {
			ok("attachment sets overlap: %s", strings.Join(shared, ", "))
		} else {
			fail("attachment sets disjoint: ours=%v stock=%v", op2.ProfAttach, sp2.ProfAttach)
		}
		note("event family ours=%q stock=%q (profiler-set, not the agent's)", op2.ProfFamily, sp2.ProfFamily)
	}

	return b.String(), pass
}

// tagValue returns the value of key in a comma-joined "k:v" tag string, "" if absent.
func tagValue(tags, key string) string {
	for _, t := range strings.Split(tags, ",") {
		if k, v, ok := strings.Cut(t, ":"); ok && k == key {
			return v
		}
	}
	return ""
}

// intersect returns the values present in both slices, order from a.
func intersect(a, b []string) []string {
	in := map[string]bool{}
	for _, s := range b {
		in[s] = true
	}
	var out []string
	for _, s := range a {
		if in[s] {
			out = append(out, s)
		}
	}
	return out
}

func checkPresence(ok, fail func(string, ...any), what string, inOurs, inStock bool) {
	switch {
	case inOurs && inStock:
		ok("%s delivered by both", what)
	default:
		fail("%s missing (ours=%v stock=%v)", what, inOurs, inStock)
	}
}

// dogstatsdValues collapses the parity-prefixed series to one phase-invariant
// scalar per context, split by what survives UDP loss. The gauge map holds the
// loss-invariant values: a plain gauge, plus the histogram aggregates (.avg,
// .median, .max, .95percentile) and the set distinct-count, all of which depend on
// the per-cycle distribution, not on how many packets arrive, so they match exactly.
// The rate map holds the run total (sum of value*interval) for the loss-sensitive
// counters and histogram .count, which scale with the number of increments received
// and so diverge when the two agents drop different UDP packets.
func dogstatsdValues(recs []record) (gauge, rate map[string]float64) {
	gauge = map[string]float64{}
	rate = map[string]float64{}
	for _, r := range recs {
		if r.Kind != "series" || !strings.HasPrefix(r.Metric, parityPrefix) {
			continue
		}
		key := metricKey(r)
		if r.Type == "rate" || r.Type == "count" {
			interval := r.Interval
			if interval < 1 {
				interval = 1
			}
			rate[key] += r.Value * float64(interval)
		} else {
			gauge[key] = r.Value
		}
	}
	return gauge, rate
}

// metricKey identifies a series by name + tag set + device. Tags are deduped and
// sorted so wire order (and our agent's lack of tag dedup) don't fragment a key, and
// the host tag is stripped so our inline host tag does not fragment it from the stock
// agent's untagged-on-the-wire series.
func metricKey(r record) string {
	key := r.Metric + "{" + strings.Join(dedupeSort(stripHostTag(r.Tags)), ",") + "}"
	if r.Device != "" {
		key += "[" + r.Device + "]"
	}
	return key
}

type logFields struct{ Source, Service, Status, Ddtags string }

// logsByMessage indexes the parity-service logs by their message content.
func logsByMessage(recs []record) map[string]logFields {
	out := map[string]logFields{}
	for _, r := range recs {
		if r.Kind == "log" && r.Service == "parity" {
			out[r.Message] = logFields{r.Source, r.Service, r.Status, r.Ddtags}
		}
	}
	return out
}

func hostTypes(recs []record) map[string]string {
	out := map[string]string{}
	for _, r := range recs {
		if r.Kind == "series" && strings.HasPrefix(r.Metric, "system.") {
			out[r.Metric] = r.Type
		}
	}
	return out
}

// hostValues maps host-level gauges (no device, only the global tag) to their
// latest value, for the stable-value tolerance checks.
func hostValues(recs []record) map[string]float64 {
	out := map[string]float64{}
	for _, r := range recs {
		if r.Kind == "series" && strings.HasPrefix(r.Metric, "system.") && r.Device == "" && len(r.Tags) <= 1 {
			out[r.Metric] = r.Value
		}
	}
	return out
}

func hasCheck(recs []record, name string) bool {
	for _, r := range recs {
		if r.Kind == "check" && r.Check == name {
			return true
		}
	}
	return false
}

func hasEvent(recs []record) bool {
	for _, r := range recs {
		if r.Kind == "event" {
			return true
		}
	}
	return false
}

func countMissing(have, want map[string]string) int {
	n := 0
	for name := range have {
		if _, ok := want[name]; !ok {
			n++
		}
	}
	return n
}

func unionKeys(a, b map[string]float64) []string {
	seen := map[string]bool{}
	var out []string
	for k := range a {
		seen[k] = true
		out = append(out, k)
	}
	for k := range b {
		if !seen[k] {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

// approxEqual treats two values as equal within a tight relative/absolute epsilon,
// enough to absorb float summation order, but not real divergence.
func approxEqual(a, b float64) bool {
	const eps = 1e-6
	return math.Abs(a-b) <= eps*math.Max(1, math.Abs(b))
}

// sameTags compares two comma-joined ddtags strings as deduped sets.
func sameTags(a, b string) bool {
	return strings.Join(dedupeSort(strings.Split(a, ",")), ",") ==
		strings.Join(dedupeSort(strings.Split(b, ",")), ",")
}

func dedupeSort(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if s != "" && !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	sort.Strings(out)
	return out
}

func trunc(s string) string {
	if len(s) > 48 {
		return s[:45] + "..."
	}
	return s
}

func firstOfKind(recs []record, kind string) (record, bool) {
	for _, r := range recs {
		if r.Kind == kind {
			return r, true
		}
	}
	return record{}, false
}

func has(recs []record, kind string) bool { _, ok := firstOfKind(recs, kind); return ok }

func pair(ours, stock []record, kind string) (record, record, bool) {
	o, ook := firstOfKind(ours, kind)
	s, sok := firstOfKind(stock, kind)
	return o, s, ook && sok
}

func hasTag(tags []string, want string) bool {
	for _, t := range tags {
		if t == want {
			return true
		}
	}
	return false
}

func compareAttr(ok, fail func(string, ...any), what, a, b string) {
	if a == b {
		ok("%s = %q", what, a)
	} else {
		fail("%s: ours=%q stock=%q", what, a, b)
	}
}

func compareNum(ok, fail func(string, ...any), what string, a, b, relTol float64) {
	if math.Abs(a-b) <= relTol*math.Abs(b) {
		ok("%s = %.0f", what, a)
	} else {
		fail("%s: ours=%.0f stock=%.0f (rel tol %.2f)", what, a, b, relTol)
	}
}

func within(a, b, relTol float64) bool { return math.Abs(a-b) <= relTol*math.Max(1, math.Abs(b)) }

// procSummary aggregates an agent's CollectorProc payloads: whether one was sent,
// the envelope encoding, and, when plain (decodable), the largest decoded list.
type procSummary struct {
	sent, decoded bool
	count         int
	encoding      int
	names         []string
}

func processSummary(recs []record) procSummary {
	var s procSummary
	for _, r := range recs {
		if r.Kind != "process" || r.ProcType != typeCollectorProc {
			continue
		}
		s.sent = true
		s.encoding = r.ProcEncoding
		if r.ProcEncoding == 0 {
			s.decoded = true
			if r.ProcCount > s.count {
				s.count, s.names = r.ProcCount, r.ProcNames
			}
		}
	}
	return s
}

func sample(ss []string, n int) []string {
	if len(ss) > n {
		return ss[:n]
	}
	return ss
}
