package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/0intro/dd-micro-agent/internal/intake"
)

// clearDDEnv unsets every ambient DD_* variable for the test, so Load-based
// tests cannot be broken by the developer's shell (t.Setenv registers the
// restoration, then the variable is removed).
func clearDDEnv(t *testing.T) {
	t.Helper()
	for _, kv := range os.Environ() {
		if k, _, _ := strings.Cut(kv, "="); strings.HasPrefix(k, "DD_") {
			t.Setenv(k, os.Getenv(k))
			os.Unsetenv(k)
		}
	}
}

func TestDefaults(t *testing.T) {
	c := Default()
	if c.Site != "datadoghq.com" {
		t.Errorf("Site = %q, want datadoghq.com", c.Site)
	}
	if c.DogstatsdPort != 8125 {
		t.Errorf("DogstatsdPort = %d, want 8125", c.DogstatsdPort)
	}
	if c.LogsConfig.BatchWait != 5 || c.LogsConfig.BatchMaxSize != 1000 || c.LogsConfig.BatchMaxContentSize != 5_000_000 {
		t.Errorf("logs defaults = %+v", c.LogsConfig)
	}
}

func TestLoadOverlaysAndIgnoresUnknownKeys(t *testing.T) {
	clearDDEnv(t)
	path := filepath.Join(t.TempDir(), "datadog.yaml")
	write(t, path, `
api_key: abc123
site: datadoghq.eu
tags:
  - env:prod
  - team:web
dogstatsd_port: 9000
logs_enabled: true
logs_config:
  batch_wait: 2
apm_config:
  enabled: true
# keys the micro-agent must ignore without error:
network_devices:
  snmp_traps: {}
`)
	c, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.APIKey != "abc123" || c.Site != "datadoghq.eu" || c.DogstatsdPort != 9000 || !c.LogsEnabled {
		t.Errorf("overlay failed: %+v", c)
	}
	if !c.APMEnabled() {
		t.Error("apm_config.enabled not parsed")
	}
	if want := []string{"env:prod", "team:web"}; !reflect.DeepEqual(c.Tags, want) {
		t.Errorf("Tags = %v, want %v", c.Tags, want)
	}
	if c.LogsConfig.BatchWait != 2 {
		t.Errorf("BatchWait = %d, want 2", c.LogsConfig.BatchWait)
	}
	if c.LogsConfig.BatchMaxSize != 1000 {
		t.Errorf("BatchMaxSize = %d, want default 1000 preserved", c.LogsConfig.BatchMaxSize)
	}
}

func TestEnvOverridesFile(t *testing.T) {
	clearDDEnv(t)
	path := filepath.Join(t.TempDir(), "datadog.yaml")
	write(t, path, "api_key: fromfile\nsite: datadoghq.com\n")
	t.Setenv("DD_API_KEY", "fromenv")
	t.Setenv("DD_TAGS", "a:1 b:2,c:3")
	t.Setenv("DD_DOGSTATSD_PORT", "7000")
	t.Setenv("DD_LOGS_ENABLED", "true")
	t.Setenv("DD_RUN_PATH", "/var/run/dd")
	t.Setenv("DD_LOGS_CONFIG_BATCH_MAX_CONTENT_SIZE", "4000000")

	c, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.APIKey != "fromenv" {
		t.Errorf("APIKey = %q, want fromenv", c.APIKey)
	}
	if want := []string{"a:1", "b:2", "c:3"}; !reflect.DeepEqual(c.Tags, want) {
		t.Errorf("Tags = %v, want %v", c.Tags, want)
	}
	if c.DogstatsdPort != 7000 || !c.LogsEnabled {
		t.Errorf("env scalars not applied: %+v", c)
	}
	if c.RunPath != "/var/run/dd" || c.LogsConfig.BatchMaxContentSize != 4000000 {
		t.Errorf("DD_RUN_PATH/DD_LOGS_CONFIG_BATCH_MAX_CONTENT_SIZE not applied: %q %d", c.RunPath, c.LogsConfig.BatchMaxContentSize)
	}
}

func TestLoadMissingFileUsesEnv(t *testing.T) {
	clearDDEnv(t)
	t.Setenv("DD_API_KEY", "k")
	c, err := Load(filepath.Join(t.TempDir(), "absent.yaml"))
	if err != nil {
		t.Fatalf("Load of missing file should not error: %v", err)
	}
	if c.APIKey != "k" || c.Site != "datadoghq.com" {
		t.Errorf("missing-file path wrong: %+v", c)
	}
}

func TestURLs(t *testing.T) {
	c := Default()
	c.Site = "datadoghq.eu"
	// The infra intake uses the agent-version-prefixed host (like the stock Agent),
	// not app.<site>. The v5 /intake/ is only processed there.
	if got, want := c.MetricsEndpoints()[0].URL, "https://0-1-0-app.agent.datadoghq.eu/api/v1/series"; got != want {
		t.Errorf("MetricsEndpoints primary = %q, want %q", got, want)
	}
	if got, want := c.IntakeEndpoints()[0].URL, "https://0-1-0-app.agent.datadoghq.eu/intake/"; got != want {
		t.Errorf("IntakeEndpoints primary = %q, want %q", got, want)
	}
	if got, want := c.LogsURL(), "https://agent-http-intake.logs.datadoghq.eu/api/v2/logs"; got != want {
		t.Errorf("LogsURL = %q, want %q", got, want)
	}
	// The process intake is process.<site>, NOT agent-version-prefixed.
	if got, want := c.ProcessURL(), "https://process.datadoghq.eu/api/v1/collector"; got != want {
		t.Errorf("ProcessURL = %q, want %q", got, want)
	}

	c.DDURL = "http://127.0.0.1:1234"
	c.LogsConfig.LogsDDURL = "127.0.0.1:5678" // no scheme -> https assumed
	c.ProcessConfig.ProcessDDURL = "https://proc.example:9000"
	if got, want := c.MetricsEndpoints()[0].URL, "http://127.0.0.1:1234/api/v1/series"; got != want {
		t.Errorf("MetricsEndpoints override = %q, want %q", got, want)
	}
	// A well-known dd_url is version-prefixed like the stock Agent's main domain,
	// so its /intake/ actually processes the v5 metadata.
	c.DDURL = "https://app.datadoghq.com"
	if got, want := c.IntakeEndpoints()[0].URL, "https://0-1-0-app.agent.datadoghq.com/intake/"; got != want {
		t.Errorf("dd_url version prefix = %q, want %q", got, want)
	}
	c.DDURL = ""
	if got, want := c.LogsURL(), "https://127.0.0.1:5678/api/v2/logs"; got != want {
		t.Errorf("LogsURL override = %q, want %q", got, want)
	}
	if got, want := c.ProcessURL(), "https://proc.example:9000/api/v1/collector"; got != want {
		t.Errorf("ProcessURL override = %q, want %q", got, want)
	}
}

// pair is a comparable (url, key, reliable) view of an Endpoint for table assertions.
type pair struct {
	url, key string
	reliable bool
}

func pairs(eps []intake.Endpoint) []pair {
	out := make([]pair, len(eps))
	for i, e := range eps {
		out[i] = pair{e.URL, e.APIKey, e.Reliable}
	}
	return out
}

func TestInfraEndpointsDualShip(t *testing.T) {
	c := Default()
	c.APIKey = "primary"
	c.AdditionalEndpoints = map[string][]string{
		"https://app.datadoghq.eu": {"eukey"},    // a Datadog domain, version-prefixed
		"https://my-proxy.example": {"proxykey"}, // a custom proxy, left as-is
	}
	// Domains are emitted in sorted order after the primary.
	got := pairs(c.MetricsEndpoints())
	want := []pair{
		{"https://0-1-0-app.agent.datadoghq.com/api/v1/series", "primary", true},
		{"https://0-1-0-app.agent.datadoghq.eu/api/v1/series", "eukey", true},
		{"https://my-proxy.example/api/v1/series", "proxykey", true},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("MetricsEndpoints =\n %+v\nwant\n %+v", got, want)
	}
	// The same set of domains carries every infra route (events/metadata ride along).
	if u := c.MetadataEndpoints()[1].URL; u != "https://0-1-0-app.agent.datadoghq.eu/api/v1/metadata" {
		t.Errorf("MetadataEndpoints additional = %q", u)
	}
}

func TestInfraEndpointsDedupPrimaryCollision(t *testing.T) {
	c := Default() // site datadoghq.com
	c.APIKey = "primary"
	// app.datadoghq.com version-prefixes to the SAME host as the implicit primary.
	c.AdditionalEndpoints = map[string][]string{
		"https://app.datadoghq.com": {"primary", "secondkey"},
	}
	got := pairs(c.MetricsEndpoints())
	want := []pair{
		{"https://0-1-0-app.agent.datadoghq.com/api/v1/series", "primary", true},   // primary, the duplicate key dropped
		{"https://0-1-0-app.agent.datadoghq.com/api/v1/series", "secondkey", true}, // a real second key, kept
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("dedup =\n %+v\nwant\n %+v", got, want)
	}
}

func TestLogsEndpointsDualShip(t *testing.T) {
	no := false
	c := Default()
	c.APIKey = "primary"
	c.LogsConfig.AdditionalEndpoints = []LogsEndpoint{
		{APIKey: "k2", Host: "agent-http-intake.logs.datadoghq.eu", Port: 443},      // reliable by default
		{APIKey: "k3", Host: "127.0.0.1", Port: 8080, UseSSL: &no, IsReliable: &no}, // http, unreliable
		{APIKey: "", Host: "skip.example"},                                          // no key -> skipped
	}
	got := pairs(c.LogsEndpoints())
	want := []pair{
		{"https://agent-http-intake.logs.datadoghq.com/api/v2/logs", "primary", true},
		{"https://agent-http-intake.logs.datadoghq.eu:443/api/v2/logs", "k2", true},
		{"http://127.0.0.1:8080/api/v2/logs", "k3", false},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("LogsEndpoints =\n %+v\nwant\n %+v", got, want)
	}
}

// The env form replaces the file value outright, like the stock Agent: an
// override that merged with the file would keep shipping to the org it was
// meant to replace.
func TestEnvAdditionalEndpointsReplaceFile(t *testing.T) {
	clearDDEnv(t)
	path := filepath.Join(t.TempDir(), "datadog.yaml")
	write(t, path, "api_key: k\nadditional_endpoints:\n  https://app.datadoghq.eu: [oldkey]\n")
	t.Setenv("DD_ADDITIONAL_ENDPOINTS", `{"https://my-proxy.example":["newkey"]}`)

	c, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	got := pairs(c.MetricsEndpoints())
	want := []pair{
		{"https://0-1-0-app.agent.datadoghq.com/api/v1/series", "k", true},
		{"https://my-proxy.example/api/v1/series", "newkey", true},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("env replace =\n %+v\nwant\n %+v (the file's eu org must be gone)", got, want)
	}
}

func TestEnvAdditionalEndpoints(t *testing.T) {
	clearDDEnv(t)
	t.Setenv("DD_API_KEY", "k")
	t.Setenv("DD_ADDITIONAL_ENDPOINTS", `{"https://app.datadoghq.eu":["envkey"]}`)
	t.Setenv("DD_LOGS_CONFIG_ADDITIONAL_ENDPOINTS", `[{"api_key":"lk","host":"h.example","port":443,"use_ssl":false}]`)
	c, err := Load(filepath.Join(t.TempDir(), "absent.yaml"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := c.MetricsEndpoints(); len(got) != 2 || got[1].URL != "https://0-1-0-app.agent.datadoghq.eu/api/v1/series" || got[1].APIKey != "envkey" {
		t.Errorf("DD_ADDITIONAL_ENDPOINTS not applied: %+v", pairs(got))
	}
	if got := c.LogsEndpoints(); len(got) != 2 || got[1].URL != "http://h.example:443/api/v2/logs" || got[1].APIKey != "lk" {
		t.Errorf("DD_LOGS_CONFIG_ADDITIONAL_ENDPOINTS not applied: %+v", pairs(got))
	}
}

func TestProcessAndProfilingEndpoints(t *testing.T) {
	no := false
	c := Default() // site datadoghq.com
	c.APIKey = "primary"
	c.ProcessConfig.AdditionalEndpoints = map[string][]string{
		"https://process.datadoghq.eu": {"euproc"},
	}
	// Process additional endpoints are not version-prefixed.
	gotP := pairs(c.ProcessEndpoints())
	wantP := []pair{
		{"https://process.datadoghq.com/api/v1/collector", "primary", true},
		{"https://process.datadoghq.eu/api/v1/collector", "euproc", true},
	}
	if !reflect.DeepEqual(gotP, wantP) {
		t.Errorf("ProcessEndpoints =\n %+v\nwant\n %+v", gotP, wantP)
	}

	// Profiling additional endpoints are full URLs (they include /api/v2/profile).
	c.APMConfig.ProfilingAdditionalEndpoints = map[string][]string{
		"https://intake.profile.datadoghq.eu/api/v2/profile": {"euprof"},
	}
	wantPr := []pair{
		{"https://intake.profile.datadoghq.com/api/v2/profile", "primary", true},
		{"https://intake.profile.datadoghq.eu/api/v2/profile", "euprof", true},
	}
	if got := pairs(c.ProfilingEndpoints()); !reflect.DeepEqual(got, wantPr) {
		t.Errorf("ProfilingEndpoints =\n %+v\nwant\n %+v", got, wantPr)
	}

	// profiling_send_to_main_endpoint: false drops the main endpoint.
	c.APMConfig.ProfilingSendToMainEndpoint = &no
	wantSkip := []pair{{"https://intake.profile.datadoghq.eu/api/v2/profile", "euprof", true}}
	if got := pairs(c.ProfilingEndpoints()); !reflect.DeepEqual(got, wantSkip) {
		t.Errorf("ProfilingEndpoints (skip main) =\n %+v\nwant\n %+v", got, wantSkip)
	}
}

func TestProcessConfig(t *testing.T) {
	if Default().ProcessEnabled() {
		t.Error("process collection should be off by default")
	}
	// The nested process_config.process_collection.enabled block parses.
	clearDDEnv(t)
	path := filepath.Join(t.TempDir(), "datadog.yaml")
	write(t, path, "api_key: k\nprocess_config:\n  process_collection:\n    enabled: true\n")
	c, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !c.ProcessEnabled() {
		t.Error("process_config.process_collection.enabled not parsed")
	}
	// Env overrides the file, for both the toggle and the URL.
	t.Setenv("DD_PROCESS_CONFIG_PROCESS_COLLECTION_ENABLED", "false")
	t.Setenv("DD_PROCESS_CONFIG_PROCESS_DD_URL", "https://p.example")
	c, err = Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.ProcessEnabled() {
		t.Error("env should disable process collection")
	}
	if got, want := c.ProcessURL(), "https://p.example/api/v1/collector"; got != want {
		t.Errorf("ProcessURL env override = %q, want %q", got, want)
	}
}

func TestProfilingConfig(t *testing.T) {
	d := Default()
	if d.APMEnabled() {
		t.Error("profiling proxy should be off by default")
	}
	if d.APMConfig.ReceiverHost != "localhost" || d.APMConfig.ReceiverPort != 8126 {
		t.Errorf("receiver defaults = %s:%d, want localhost:8126", d.APMConfig.ReceiverHost, d.APMConfig.ReceiverPort)
	}
	if d.DefaultEnv() != "none" {
		t.Errorf("DefaultEnv = %q, want none", d.DefaultEnv())
	}

	c := Default()
	c.Site = "datadoghq.eu"
	if got, want := c.ProfilingURL(), "https://intake.profile.datadoghq.eu/api/v2/profile"; got != want {
		t.Errorf("ProfilingURL = %q, want %q", got, want)
	}
	c.APMConfig.ProfilingDDURL = "http://127.0.0.1:18080/api/v2/profile"
	if got, want := c.ProfilingURL(), "http://127.0.0.1:18080/api/v2/profile"; got != want {
		t.Errorf("ProfilingURL override = %q, want %q", got, want)
	}

	// The apm_config block and env overrides parse.
	clearDDEnv(t)
	path := filepath.Join(t.TempDir(), "datadog.yaml")
	write(t, path, "api_key: k\nenv: prod\napm_config:\n  enabled: true\n  receiver_port: 9126\n")
	c, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !c.APMEnabled() || c.APMConfig.ReceiverPort != 9126 || c.DefaultEnv() != "prod" {
		t.Errorf("apm_config not parsed: %+v env=%q", c.APMConfig, c.DefaultEnv())
	}
	t.Setenv("DD_APM_ENABLED", "false")
	t.Setenv("DD_APM_RECEIVER_PORT", "7126")
	t.Setenv("DD_APM_PROFILING_DD_URL", "https://prof.example/api/v2/profile")
	c, err = Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.APMEnabled() {
		t.Error("env should disable the profiling proxy")
	}
	if c.APMConfig.ReceiverPort != 7126 {
		t.Errorf("DD_APM_RECEIVER_PORT not applied: %d", c.APMConfig.ReceiverPort)
	}
	if got, want := c.ProfilingURL(), "https://prof.example/api/v2/profile"; got != want {
		t.Errorf("ProfilingURL env override = %q, want %q", got, want)
	}
}

func TestValidate(t *testing.T) {
	if Default().Validate() == nil {
		t.Error("Validate should fail with empty api_key")
	}
	c := Default()
	c.APIKey = "x"
	if err := c.Validate(); err != nil {
		t.Errorf("Validate with key: %v", err)
	}
}

func TestLoadLogSources(t *testing.T) {
	confd := filepath.Join(t.TempDir(), "conf.d")
	mkdir(t, filepath.Join(confd, "nginx.d"))
	write(t, filepath.Join(confd, "nginx.d", "conf.yaml"), `
logs:
  - type: file
    path: /var/log/nginx/access.log
    service: nginx
    source: nginx
    tags: [team:web]
  - type: tcp
    port: 10514
`)

	sources, skipped, err := LoadLogSources(confd)
	if err != nil {
		t.Fatalf("LoadLogSources: %v", err)
	}
	if len(sources) != 1 {
		t.Fatalf("got %d sources, want 1: %+v", len(sources), sources)
	}
	got := sources[0]
	want := LogSource{Type: "file", Path: "/var/log/nginx/access.log", Service: "nginx", Source: "nginx", Tags: []string{"team:web"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("source = %+v, want %+v", got, want)
	}
	if len(skipped) != 1 {
		t.Errorf("skipped = %v, want one tcp entry", skipped)
	}
}

func TestLoadLogSourcesJournald(t *testing.T) {
	confd := filepath.Join(t.TempDir(), "conf.d")
	mkdir(t, filepath.Join(confd, "journald.d"))
	write(t, filepath.Join(confd, "journald.d", "conf.yaml"), `
logs:
  - type: journald
    source: systemd
    start_position: beginning
    include_units: [ssh.service, cron.service]
    exclude_units: ["*"]
    include_matches: ["PRIORITY=3"]
  - type: udp
    port: 10514
`)

	sources, skipped, err := LoadLogSources(confd)
	if err != nil {
		t.Fatalf("LoadLogSources: %v", err)
	}
	if len(sources) != 1 {
		t.Fatalf("got %d sources, want 1: %+v", len(sources), sources)
	}
	got := sources[0]
	want := LogSource{
		Type:           "journald",
		Source:         "systemd",
		StartPosition:  "beginning",
		IncludeUnits:   []string{"ssh.service", "cron.service"},
		ExcludeUnits:   []string{"*"},
		IncludeMatches: []string{"PRIORITY=3"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("source = %+v, want %+v", got, want)
	}
	if len(skipped) != 1 {
		t.Errorf("skipped = %v, want one udp entry", skipped)
	}
}

func TestLoadLogSourcesMissingDir(t *testing.T) {
	sources, _, err := LoadLogSources(filepath.Join(t.TempDir(), "nonexistent"))
	if err != nil || sources != nil {
		t.Errorf("missing confd: sources=%v err=%v, want nil,nil", sources, err)
	}
}

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func mkdir(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatal(err)
	}
}
