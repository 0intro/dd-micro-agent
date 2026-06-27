package main

import "testing"

// a recording that satisfies every assertion below.
func goodRecords() []record {
	return []record{
		{Kind: "series", Metric: "system.mem.total", Type: "gauge", Host: "h1"},
		{Kind: "series", Metric: "system.load.1", Type: "gauge"},
		{Kind: "meta", Attrs: map[string]string{"platform": "freebsd", "hostname": "h1"}},
		{Kind: "log", Message: "GET /index.html 200", Service: "nginx"},
		{Kind: "process", ProcType: typeCollectorProc, ProcEncoding: 0, ProcCount: 12,
			ProcNames: []string{"agent", "init", "cs"}},
	}
}

func TestCheckRecordsAllPass(t *testing.T) {
	o := verifyOpts{
		series:   []string{"system.mem.total", "system.load.1"},
		platform: "freebsd",
		minProcs: 10,
		procName: "agent",
		logSub:   "GET /",
		host:     "h1",
		wantMeta: true,
	}
	if report, pass := checkRecords(goodRecords(), o); !pass {
		t.Errorf("expected pass, got fail:\n%s", report)
	}
}

func TestCheckRecordsFailures(t *testing.T) {
	cases := map[string]verifyOpts{
		"absent series":    {series: []string{"system.cpu.user"}},
		"wrong platform":   {platform: "linux"},
		"too few procs":    {minProcs: 100},
		"absent proc":      {procName: "nginx"},
		"absent log":       {logSub: "POST /"},
		"absent host":      {host: "other"},
		"missing metadata": {wantMeta: true},
	}
	recs := goodRecords()
	for name, o := range cases {
		if name == "missing metadata" {
			recs = []record{{Kind: "series", Metric: "system.mem.total"}} // no meta/inv
		} else {
			recs = goodRecords()
		}
		if _, pass := checkRecords(recs, o); pass {
			t.Errorf("%s: expected fail, got pass", name)
		}
	}
}

func TestCheckRecordsProcNameAcrossChunks(t *testing.T) {
	// A collect chunks at 100 procs/msg, so the wanted name can be in a later record
	// than the largest one (as on FreeBSD, where 100 kernel threads fill the first chunk).
	recs := []record{
		{Kind: "process", ProcType: typeCollectorProc, ProcEncoding: 0, ProcCount: 100,
			ProcNames: []string{"kernel", "init"}},
		{Kind: "process", ProcType: typeCollectorProc, ProcEncoding: 0, ProcCount: 20,
			ProcNames: []string{"sshd", "agent"}},
	}
	if report, pass := checkRecords(recs, verifyOpts{procName: "agent", minProcs: 50}); !pass {
		t.Errorf("expected agent (in a later chunk) found and min-procs met:\n%s", report)
	}
}

func TestCheckRecordsProcessNotDecodable(t *testing.T) {
	recs := []record{{Kind: "process", ProcType: typeCollectorProc, ProcEncoding: 2}} // zstd, opaque
	if _, pass := checkRecords(recs, verifyOpts{minProcs: 1}); pass {
		t.Error("expected fail when CollectorProc is recorded but not decodable")
	}
}

func TestCheckRecordsProfile(t *testing.T) {
	good := []record{{Kind: "profile", ProfVia: "trace-agent 0.1.0", ProfAPIKey: true,
		ProfAddTags: "host:h1,default_env:none,agent_version:0.1.0", ProfFamily: "go",
		ProfAttach: []string{"cpu.pprof", "heap.pprof"}}}

	if report, pass := checkRecords(good, verifyOpts{profile: true, profileFamily: "go", profileAttach: "cpu.pprof"}); !pass {
		t.Errorf("well-formed profile should pass:\n%s", report)
	}
	if _, pass := checkRecords(nil, verifyOpts{profile: true}); pass {
		t.Error("no profile record should fail")
	}
	bad := []record{{Kind: "profile"}} // forwarded but missing the injected headers
	if _, pass := checkRecords(bad, verifyOpts{profile: true}); pass {
		t.Error("profile without proxy headers should fail")
	}
	if _, pass := checkRecords(good, verifyOpts{profileFamily: "native"}); pass {
		t.Error("wrong family should fail")
	}
	if _, pass := checkRecords(good, verifyOpts{profileAttach: "mutex.pprof"}); pass {
		t.Error("absent attachment should fail")
	}
}
