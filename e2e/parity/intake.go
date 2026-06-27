package main

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
)

// record is one normalized item decoded from an agent's intake traffic. The
// per-agent JSONL recordings (one record per line) are the comparison input.
type record struct {
	Kind string `json:"kind"` // series | log | check | event | meta | inv_host | inv_agent | process

	// APIKey is the DD-API-KEY header the agent sent, except for the v5 /intake/
	// envelope (host metadata and events) where it is the apiKey embedded in the
	// body, the only end-to-end proof that dual-shipping rebuilds the body per org.
	APIKey string `json:"api_key,omitempty"`

	// series
	Metric   string   `json:"metric,omitempty"`
	Type     string   `json:"type,omitempty"` // gauge | rate | count
	Value    float64  `json:"value,omitempty"`
	Interval int64    `json:"interval,omitempty"`
	Tags     []string `json:"tags,omitempty"`
	Host     string   `json:"host,omitempty"`
	Device   string   `json:"device,omitempty"`

	// log
	Message string `json:"message,omitempty"`
	Service string `json:"service,omitempty"`
	Source  string `json:"source,omitempty"`
	Status  string `json:"status,omitempty"`
	Ddtags  string `json:"ddtags,omitempty"`

	// check / event
	Check       string `json:"check,omitempty"`
	CheckStatus int    `json:"check_status,omitempty"`
	Title       string `json:"title,omitempty"`

	// host metadata / inventory: comparable facts keyed by a canonical name
	Attrs map[string]string  `json:"attrs,omitempty"`
	Nums  map[string]float64 `json:"nums,omitempty"`

	// process (Live Processes): the MessageV3 envelope + decoded list (plain only)
	ProcType     int      `json:"proc_type,omitempty"`
	ProcEncoding int      `json:"proc_encoding,omitempty"` // 0 = plain protobuf (decodable), else compressed
	ProcCount    int      `json:"proc_count,omitempty"`
	ProcNames    []string `json:"proc_names,omitempty"`

	// profile (continuous profiling): the event.json facts and attachment names
	// from the forwarded multipart, plus the headers the proxy injects (the agent's
	// own contribution, which is what parity compares). The pprof bytes are opaque.
	ProfFamily    string   `json:"prof_family,omitempty"`
	ProfVersion   string   `json:"prof_version,omitempty"`
	ProfAttach    []string `json:"prof_attach,omitempty"`
	ProfTags      string   `json:"prof_tags,omitempty"` // tags_profiler, set by the profiler
	ProfVia       string   `json:"prof_via,omitempty"`
	ProfAddTags   string   `json:"prof_add_tags,omitempty"` // X-Datadog-Additional-Tags
	ProfUserAgent string   `json:"prof_user_agent,omitempty"`
	ProfAPIKey    bool     `json:"prof_api_key,omitempty"`
}

// recorder serializes records to one file. HTTP handlers run concurrently.
type recorder struct {
	mu  sync.Mutex
	enc *json.Encoder
}

func (r *recorder) write(rec record) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.enc.Encode(rec) // json.Encoder appends a newline -> JSONL
}

// writer is what the decoders write through. The handler wraps the recorder per
// request so every record is stamped with that request's API key.
type writer interface {
	write(record)
}

// taggedRecorder stamps each record with the request's DD-API-KEY header unless the
// decoder already set a key (the v5 /intake/ decoders set the body apiKey instead).
type taggedRecorder struct {
	*recorder
	apiKey string
}

func (t *taggedRecorder) write(rec record) {
	if rec.APIKey == "" {
		rec.APIKey = t.apiKey
	}
	t.recorder.write(rec)
}

// serve runs an HTTP recorder per "LABEL=ADDR" arg, writing <dir>/<label>.jsonl
// until interrupted. Both agents post here. Isolation is by port (label).
func serve(args []string) {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	dir := fs.String("dir", ".", "directory for <label>.jsonl recordings")
	fs.Parse(args)
	if fs.NArg() == 0 {
		fmt.Fprintln(os.Stderr, "serve: need at least one LABEL=ADDR")
		os.Exit(2)
	}
	if err := os.MkdirAll(*dir, 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "mkdir %s: %v\n", *dir, err)
		os.Exit(1)
	}

	var servers []*http.Server
	for _, spec := range fs.Args() {
		label, addr, ok := strings.Cut(spec, "=")
		if !ok {
			fmt.Fprintf(os.Stderr, "serve: bad LABEL=ADDR %q\n", spec)
			os.Exit(2)
		}
		f, err := os.Create(filepath.Join(*dir, label+".jsonl"))
		if err != nil {
			fmt.Fprintf(os.Stderr, "create recording: %v\n", err)
			os.Exit(1)
		}
		defer f.Close()
		rec := &recorder{enc: json.NewEncoder(f)}
		mux := http.NewServeMux()
		mux.HandleFunc("/", func(w http.ResponseWriter, req *http.Request) { handle(rec, w, req) })
		srv := &http.Server{Addr: addr, Handler: mux}
		servers = append(servers, srv)
		go func() {
			if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				// A dead listener records nothing and every later assertion
				// fails obscurely, so stop here, loudly.
				fmt.Fprintf(os.Stderr, "listen %s: %v\n", addr, err)
				os.Exit(1)
			}
		}()
		fmt.Fprintf(os.Stderr, "parity serve: %s -> %s/%s.jsonl\n", addr, *dir, label)
	}

	// Block until interrupted. Records are written synchronously (each Encode is a
	// file write), so even a default SIGTERM leaves a complete recording. This just
	// allows a clean Ctrl-C. os.Interrupt keeps the tool portable (no syscall).
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt)
	<-stop
	for _, s := range servers {
		s.Close()
	}
}

func handle(rec *recorder, w http.ResponseWriter, req *http.Request) {
	body, err := readBody(req)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	tr := &taggedRecorder{recorder: rec, apiKey: req.Header.Get("DD-API-KEY")}
	switch req.URL.Path {
	case "/api/v1/series":
		decodeSeries(tr, body)
	case "/api/v2/logs":
		decodeLogs(tr, body)
	case "/api/v1/check_run":
		decodeChecks(tr, body)
	case "/api/v1/metadata":
		decodeInventory(tr, body)
	case "/api/v1/collector":
		decodeProcess(tr, body)
	case "/api/v2/profile":
		decodeProfile(tr, req, body)
	case "/intake/":
		decodeIntake(tr, body)
	}
	w.WriteHeader(http.StatusAccepted)
}

// readBody returns the request body, gunzipped when the agent compressed it. The
// process payload is the one path with no HTTP-layer compression (its framing
// carries the encoding instead), which the gzip header check handles.
func readBody(req *http.Request) ([]byte, error) {
	var r io.Reader = req.Body
	if req.Header.Get("Content-Encoding") == "gzip" {
		gr, err := gzip.NewReader(req.Body)
		if err != nil {
			return nil, err
		}
		defer gr.Close()
		r = gr
	}
	return io.ReadAll(io.LimitReader(r, 32<<20))
}

func decodeSeries(rec writer, body []byte) {
	var p struct {
		Series []struct {
			Metric   string      `json:"metric"`
			Type     string      `json:"type"`
			Interval int64       `json:"interval"`
			Tags     []string    `json:"tags"`
			Host     string      `json:"host"`
			Device   string      `json:"device"`
			Points   [][]float64 `json:"points"`
		} `json:"series"`
	}
	if json.Unmarshal(body, &p) != nil {
		return
	}
	for _, s := range p.Series {
		var v float64
		if n := len(s.Points); n > 0 && len(s.Points[n-1]) == 2 {
			v = s.Points[n-1][1] // last point's value
		}
		rec.write(record{
			Kind: "series", Metric: s.Metric, Type: s.Type, Value: v,
			Interval: s.Interval, Tags: s.Tags, Host: s.Host, Device: s.Device,
		})
	}
}

func decodeLogs(rec writer, body []byte) {
	var arr []struct {
		Message string `json:"message"`
		Status  string `json:"status"`
		Service string `json:"service"`
		Source  string `json:"ddsource"`
		Ddtags  string `json:"ddtags"`
		Host    string `json:"hostname"`
	}
	if json.Unmarshal(body, &arr) != nil {
		return
	}
	for _, l := range arr {
		rec.write(record{
			Kind: "log", Message: l.Message, Status: l.Status,
			Service: l.Service, Source: l.Source, Ddtags: l.Ddtags, Host: l.Host,
		})
	}
}

func decodeChecks(rec writer, body []byte) {
	var arr []struct {
		Check  string `json:"check"`
		Status int    `json:"status"`
		Host   string `json:"host_name"`
	}
	if json.Unmarshal(body, &arr) != nil {
		return
	}
	for _, c := range arr {
		rec.write(record{Kind: "check", Check: c.Check, CheckStatus: c.Status, Host: c.Host})
	}
}

// decodeIntake records either an events envelope or the v5 host-metadata payload.
// Both land on /intake/. Events have an "events" map. Metadata has systemStats/gohai.
func decodeIntake(rec writer, body []byte) {
	var ev struct {
		APIKey string `json:"apiKey"`
		Events map[string][]struct {
			Title string `json:"msg_title"`
		} `json:"events"`
	}
	if json.Unmarshal(body, &ev) == nil && ev.Events != nil {
		for _, evs := range ev.Events {
			for _, e := range evs {
				rec.write(record{Kind: "event", Title: e.Title, APIKey: ev.APIKey})
			}
		}
		return
	}
	decodeV5Meta(rec, body)
}

// decodeV5Meta extracts the comparable facts from the v5 host-metadata payload:
// systemStats.platform (drives the OS icon), the hostname, the host tags, and the
// cpu/memory facts from the embedded (JSON-string) gohai block.
func decodeV5Meta(rec writer, body []byte) {
	var p struct {
		APIKey      string `json:"apiKey"`
		OS          string `json:"os"`
		SystemStats struct {
			Platform string `json:"platform"`
			CPUCores int    `json:"cpuCores"`
		} `json:"systemStats"`
		Meta struct {
			Hostname string `json:"hostname"`
		} `json:"meta"`
		HostTags struct {
			System []string `json:"system"`
		} `json:"host-tags"`
		Gohai string `json:"gohai"`
	}
	if json.Unmarshal(body, &p) != nil || (p.SystemStats.Platform == "" && p.Gohai == "") {
		return
	}
	nums := map[string]float64{"cpu_cores": float64(p.SystemStats.CPUCores)}
	var g struct {
		CPU struct {
			CPUCores uint64 `json:"cpu_cores"`
		} `json:"cpu"`
		Memory struct {
			Total uint64 `json:"total"`
		} `json:"memory"`
	}
	if json.Unmarshal([]byte(p.Gohai), &g) == nil {
		if g.CPU.CPUCores > 0 {
			nums["cpu_cores"] = float64(g.CPU.CPUCores)
		}
		nums["mem_total"] = float64(g.Memory.Total)
	}
	rec.write(record{
		Kind:   "meta",
		APIKey: p.APIKey,
		Attrs:  map[string]string{"os": p.OS, "platform": p.SystemStats.Platform, "hostname": p.Meta.Hostname},
		Nums:   nums,
		Tags:   p.HostTags.System,
	})
}

// decodeInventory records the modern inventory_host / inventory_agent payloads
// (each /api/v1/metadata POST carries one of them).
func decodeInventory(rec writer, body []byte) {
	var p struct {
		HostMetadata *struct {
			OS            string `json:"os"`
			KernelName    string `json:"kernel_name"`
			CPUArch       string `json:"cpu_architecture"`
			CPUCores      uint64 `json:"cpu_cores"`
			CPULogical    uint64 `json:"cpu_logical_processors"`
			MemoryTotalKB uint64 `json:"memory_total_kb"`
			AgentVersion  string `json:"agent_version"`
		} `json:"host_metadata"`
		AgentMetadata map[string]any `json:"agent_metadata"`
	}
	if json.Unmarshal(body, &p) != nil {
		return
	}
	if h := p.HostMetadata; h != nil {
		rec.write(record{
			Kind: "inv_host",
			Attrs: map[string]string{
				"os": h.OS, "kernel_name": h.KernelName,
				"cpu_architecture": h.CPUArch, "agent_version": h.AgentVersion,
			},
			Nums: map[string]float64{
				"cpu_cores": float64(h.CPUCores), "cpu_logical": float64(h.CPULogical),
				"memory_total_kb": float64(h.MemoryTotalKB),
			},
		})
	}
	if m := p.AgentMetadata; m != nil {
		av, _ := m["agent_version"].(string)
		fl, _ := m["flavor"].(string)
		rec.write(record{Kind: "inv_agent", Attrs: map[string]string{"agent_version": av, "flavor": fl}})
	}
}

// decodeProcess reads the MessageV3 frame an agent POSTs to /api/v1/collector. The
// 16-byte header (version, encoding, type, ...) is never compressed, so the type
// and encoding are always readable. The body is decoded only when it is plain
// protobuf (encoding 0). The stock agent's zstd body is opaque to us.
func decodeProcess(rec writer, body []byte) {
	if len(body) < 16 {
		return
	}
	r := record{Kind: "process", ProcType: int(body[2]), ProcEncoding: int(body[1])}
	if r.ProcEncoding == 0 && r.ProcType == typeCollectorProc {
		r.ProcCount, r.ProcNames = decodeCollectorProc(body[16:])
	}
	rec.write(r)
}

// decodeProfile records a forwarded profiling upload. From the multipart body it
// keeps the event.json facts and the attachment filenames, and from the request it
// keeps the headers the proxy injects (Via, X-Datadog-Additional-Tags, the API key
// presence). The gzipped pprof attachments are opaque, so only their names are kept.
func decodeProfile(rec writer, req *http.Request, body []byte) {
	_, params, err := mime.ParseMediaType(req.Header.Get("Content-Type"))
	if err != nil || params["boundary"] == "" {
		return
	}
	r := record{
		Kind:          "profile",
		ProfVia:       req.Header.Get("Via"),
		ProfAddTags:   req.Header.Get("X-Datadog-Additional-Tags"),
		ProfUserAgent: req.Header.Get("User-Agent"),
		ProfAPIKey:    req.Header.Get("DD-API-KEY") != "",
	}
	mr := multipart.NewReader(bytes.NewReader(body), params["boundary"])
	for {
		part, err := mr.NextPart()
		if err != nil {
			break
		}
		if part.FormName() == "event" || part.FileName() == "event.json" {
			var ev struct {
				Family      string   `json:"family"`
				Version     string   `json:"version"`
				Attachments []string `json:"attachments"`
				Tags        string   `json:"tags_profiler"`
			}
			data, _ := io.ReadAll(part)
			if json.Unmarshal(data, &ev) == nil {
				r.ProfFamily, r.ProfVersion, r.ProfTags = ev.Family, ev.Version, ev.Tags
			}
		} else if name := part.FileName(); name != "" {
			r.ProfAttach = append(r.ProfAttach, name)
		}
		part.Close()
	}
	rec.write(r)
}

// loadRecords reads a JSONL recording written by serve.
func loadRecords(path string) ([]record, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []record
	dec := json.NewDecoder(f)
	for {
		var r record
		err := dec.Decode(&r)
		if err == io.EOF {
			return out, nil
		}
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
}
