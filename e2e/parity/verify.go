package main

import (
	"flag"
	"fmt"
	"os"
	"strings"
)

// verify asserts that a single recording (one agent against the fake intake)
// contains the records the caller expects. Unlike compare it needs no second
// recording, so the per-OS VM e2e tests can run against the fake intake with no
// stock agent, no pup, and no real Datadog. Each flag that is set adds one
// assertion. It loads the recording, runs the checks, and returns the exit code.
func verify(args []string) int {
	fs := flag.NewFlagSet("verify", flag.ExitOnError)
	series := fs.String("series", "", "comma-separated metric names that must be present")
	platform := fs.String("platform", "", "required host-metadata platform (systemStats.platform or inventory os)")
	minProcs := fs.Int("min-procs", 0, "minimum decoded CollectorProc process count")
	procName := fs.String("proc-name", "", "a process name that must appear in the Live Processes list")
	logSub := fs.String("log", "", "a substring that must appear in some shipped log message")
	host := fs.String("host", "", "a host name that must appear on a series or host metadata")
	apiKey := fs.String("api-key", "", "every record must carry this DD-API-KEY (header, or v5 body apiKey)")
	wantMeta := fs.Bool("meta", false, "require host metadata (v5 or inventory) to have been sent")
	profile := fs.Bool("profile", false, "require a forwarded profiling upload with the injected proxy headers")
	profileFamily := fs.String("profile-family", "", "required profile event family (go or native)")
	profileAttach := fs.String("profile-attach", "", "a pprof attachment name that must be present")
	checkName := fs.String("check", "", "a DogStatsD service-check name that must have been sent")
	eventSub := fs.String("event", "", "a substring that must appear in some DogStatsD event title")
	fs.Parse(args)
	if fs.NArg() != 1 {
		fmt.Fprintln(os.Stderr, "usage: parity verify [flags] RECORDING.jsonl")
		return 2
	}
	recs, err := loadRecords(fs.Arg(0))
	if err != nil {
		fmt.Fprintf(os.Stderr, "load %s: %v\n", fs.Arg(0), err)
		return 2
	}
	opts := verifyOpts{
		series:        splitComma(*series),
		platform:      *platform,
		minProcs:      *minProcs,
		procName:      *procName,
		logSub:        *logSub,
		host:          *host,
		apiKey:        *apiKey,
		wantMeta:      *wantMeta,
		profile:       *profile,
		profileFamily: *profileFamily,
		profileAttach: *profileAttach,
		checkName:     *checkName,
		eventSub:      *eventSub,
	}
	report, pass := checkRecords(recs, opts)
	fmt.Print(report)
	if pass {
		fmt.Println("==> VERIFY PASS")
		return 0
	}
	fmt.Println("==> VERIFY FAIL")
	return 1
}

// verifyOpts is the set of assertions to run against a recording. A zero field is
// skipped, so the caller asserts only what its OS scenario produces.
type verifyOpts struct {
	series        []string
	platform      string
	minProcs      int
	procName      string
	logSub        string
	host          string
	apiKey        string
	wantMeta      bool
	profile       bool
	profileFamily string
	profileAttach string
	checkName     string
	eventSub      string
}

// checkRecords runs each requested assertion against the recording and returns a
// human report plus the overall pass/fail. It is pure so verify_test.go exercises
// it without a recording file, the same pattern as compare.
func checkRecords(recs []record, o verifyOpts) (string, bool) {
	var b strings.Builder
	pass, checked := true, false
	ok := func(f string, a ...any) { checked = true; fmt.Fprintf(&b, "  ok    "+f+"\n", a...) }
	fail := func(f string, a ...any) { checked = true; pass = false; fmt.Fprintf(&b, "  FAIL  "+f+"\n", a...) }

	names := seriesNames(recs)
	for _, m := range o.series {
		if names[m] {
			ok("series %s present", m)
		} else {
			fail("series %s absent", m)
		}
	}
	if o.platform != "" {
		if got := metaPlatform(recs); got == o.platform {
			ok("host metadata platform = %q", got)
		} else {
			fail("host metadata platform = %q, want %q", got, o.platform)
		}
	}
	if o.wantMeta {
		if has(recs, "meta") || has(recs, "inv_host") {
			ok("host metadata sent")
		} else {
			fail("no host metadata (v5 or inventory) recorded")
		}
	}
	if o.minProcs > 0 || o.procName != "" {
		ps := processSummary(recs)
		switch {
		case !ps.sent:
			fail("no CollectorProc recorded")
		case !ps.decoded:
			fail("CollectorProc recorded but not decodable (encoding=%d)", ps.encoding)
		default:
			if o.minProcs > 0 {
				if ps.count >= o.minProcs {
					ok("process list has %d processes (>= %d)", ps.count, o.minProcs)
				} else {
					fail("process list has %d processes (< %d)", ps.count, o.minProcs)
				}
			}
			if o.procName != "" {
				// A collect chunks at 100 procs/msg, so a name can land in a later
				// record than the largest one. Union the names across all chunks.
				names := allProcNames(recs)
				if hasTag(names, o.procName) {
					ok("process %q present", o.procName)
				} else {
					fail("process %q absent (have e.g. %s)", o.procName, strings.Join(sample(names, 8), ", "))
				}
			}
		}
	}
	if o.logSub != "" {
		if logContains(recs, o.logSub) {
			ok("log containing %q present", trunc(o.logSub))
		} else {
			fail("no log message contains %q", trunc(o.logSub))
		}
	}
	if o.host != "" {
		if hostPresent(recs, o.host) {
			ok("host %q present on series/metadata", o.host)
		} else {
			fail("host %q absent from series/metadata", o.host)
		}
	}
	if o.apiKey != "" {
		// Every record this org received must carry this org's key: the header for
		// series/logs/checks/inventory/process, the body apiKey for the v5 /intake/
		// envelope. A wrong key means a dual-shipping fan-out bug.
		keyed, empty, wrong := 0, 0, ""
		for _, r := range recs {
			if r.APIKey == "" {
				empty++ // every decoder stamps a key, so empty means a fan-out bug
				continue
			}
			keyed++
			if r.APIKey != o.apiKey {
				wrong = r.APIKey
				break
			}
		}
		switch {
		case keyed == 0:
			fail("no record carried an API key")
		case wrong != "":
			fail("a record carried key %q, want %q (wrong org)", trunc(wrong), trunc(o.apiKey))
		case empty > 0:
			fail("%d records carried no API key", empty)
		default:
			ok("all %d keyed records carry the expected API key", keyed)
		}
	}
	if o.profile || o.profileFamily != "" || o.profileAttach != "" {
		p, found := firstOfKind(recs, "profile")
		switch {
		case !found:
			fail("no profiling upload forwarded")
		default:
			if strings.HasPrefix(p.ProfVia, "trace-agent ") && p.ProfAPIKey && strings.Contains(p.ProfAddTags, "host:") {
				ok("profile forwarded with Via, DD-API-KEY, and host tag")
			} else {
				fail("forwarded profile missing proxy headers (via=%q api_key=%v add_tags=%q)", p.ProfVia, p.ProfAPIKey, p.ProfAddTags)
			}
			if o.profileFamily != "" {
				if p.ProfFamily == o.profileFamily {
					ok("profile family = %q", p.ProfFamily)
				} else {
					fail("profile family = %q, want %q", p.ProfFamily, o.profileFamily)
				}
			}
			if o.profileAttach != "" {
				if hasTag(p.ProfAttach, o.profileAttach) {
					ok("attachment %q present", o.profileAttach)
				} else {
					fail("attachment %q absent (have %s)", o.profileAttach, strings.Join(p.ProfAttach, ", "))
				}
			}
		}
	}
	if o.checkName != "" {
		if checkPresent(recs, o.checkName) {
			ok("service check %q present", o.checkName)
		} else {
			fail("service check %q absent", o.checkName)
		}
	}
	if o.eventSub != "" {
		if eventContains(recs, o.eventSub) {
			ok("event title containing %q present", o.eventSub)
		} else {
			fail("no event title contains %q", o.eventSub)
		}
	}
	if !checked {
		pass = false
		fmt.Fprintf(&b, "  FAIL  nothing to verify (no assertion was requested)\n")
	}
	return b.String(), pass
}

// allProcNames unions the decoded process names across every CollectorProc record.
// The collector chunks at 100 procs per message, so one collect spans several
// records and a given process can appear in any of them.
func allProcNames(recs []record) []string {
	seen := map[string]bool{}
	var out []string
	for _, r := range recs {
		if r.Kind == "process" && r.ProcEncoding == 0 {
			for _, n := range r.ProcNames {
				if !seen[n] {
					seen[n] = true
					out = append(out, n)
				}
			}
		}
	}
	return out
}

func seriesNames(recs []record) map[string]bool {
	out := map[string]bool{}
	for _, r := range recs {
		if r.Kind == "series" {
			out[r.Metric] = true
		}
	}
	return out
}

// metaPlatform returns the platform from the v5 host metadata, falling back to the
// inventory os field.
func metaPlatform(recs []record) string {
	if m, ok := firstOfKind(recs, "meta"); ok {
		if p := m.Attrs["platform"]; p != "" {
			return p
		}
	}
	if m, ok := firstOfKind(recs, "inv_host"); ok {
		return m.Attrs["os"]
	}
	return ""
}

func logContains(recs []record, sub string) bool {
	for _, r := range recs {
		if r.Kind == "log" && strings.Contains(r.Message, sub) {
			return true
		}
	}
	return false
}

// checkPresent reports whether a DogStatsD service check with the given name was sent
// (the _sc path: parse → aggregate → /api/v1/check_run).
func checkPresent(recs []record, name string) bool {
	for _, r := range recs {
		if r.Kind == "check" && r.Check == name {
			return true
		}
	}
	return false
}

// eventContains reports whether some DogStatsD event's title contains sub (the _e path:
// parse → aggregate → the legacy v5 /intake/ envelope).
func eventContains(recs []record, sub string) bool {
	for _, r := range recs {
		if r.Kind == "event" && strings.Contains(r.Title, sub) {
			return true
		}
	}
	return false
}

func hostPresent(recs []record, host string) bool {
	for _, r := range recs {
		if r.Host == host {
			return true
		}
		if r.Kind == "meta" && r.Attrs["hostname"] == host {
			return true
		}
	}
	return false
}

func splitComma(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
