// Package config loads the subset of datadog.yaml (and conf.d integration
// configs) that the micro-agent needs for logs and metrics. Every other key in
// the file is ignored: yaml.v3 silently drops fields that have no matching
// struct member, which is exactly the behavior we want.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/0intro/dd-micro-agent/internal/intake"
)

// Config is the resolved agent configuration: datadog.yaml overlaid on the
// defaults, with DD_* environment variables applied last.
type Config struct {
	APIKey                   string              `yaml:"api_key"`
	Site                     string              `yaml:"site"`
	DDURL                    string              `yaml:"dd_url"`
	AdditionalEndpoints      map[string][]string `yaml:"additional_endpoints"` // dual-shipping: extra infra (url -> keys)
	Env                      string              `yaml:"env"`
	Hostname                 string              `yaml:"hostname"`
	HostnameFile             string              `yaml:"hostname_file"`
	Tags                     []string            `yaml:"tags"`
	DogstatsdPort            int                 `yaml:"dogstatsd_port"`
	DogstatsdSocket          string              `yaml:"dogstatsd_socket"`
	DogstatsdNonLocalTraffic bool                `yaml:"dogstatsd_non_local_traffic"`
	LogsEnabled              bool                `yaml:"logs_enabled"`
	LogsConfig               LogsAgentConfig     `yaml:"logs_config"`
	Proxy                    Proxy               `yaml:"proxy"`
	SkipSSLValidation        bool                `yaml:"skip_ssl_validation"`
	ConfdPath                string              `yaml:"confd_path"`
	RunPath                  string              `yaml:"run_path"`
	EnableMetadataCollection bool                `yaml:"enable_metadata_collection"`
	VentiURL                 string              `yaml:"venti_url"`      // opt-in Plan 9 venti disk metrics, empty = off
	ProcessConfig            ProcessConfig       `yaml:"process_config"` // opt-in Live Processes, disabled by default
	APMConfig                APMConfig           `yaml:"apm_config"`     // opt-in profiling proxy, disabled by default
}

// APMConfig mirrors the datadog.yaml `apm_config:` block. The micro-agent
// implements only the profiling proxy from the trace-agent, so it reads the keys
// that govern where it listens and where it forwards profiles. receiver_host is
// agent-specific: the stock trace-agent takes its bind address from the
// top-level bind_host instead.
type APMConfig struct {
	Enabled                      bool                `yaml:"enabled"`
	ReceiverHost                 string              `yaml:"receiver_host"`
	ReceiverPort                 int                 `yaml:"receiver_port"`
	NonLocalTraffic              bool                `yaml:"apm_non_local_traffic"`
	ProfilingDDURL               string              `yaml:"profiling_dd_url"`
	ProfilingAdditionalEndpoints map[string][]string `yaml:"profiling_additional_endpoints"`  // dual-shipping (url -> keys)
	ProfilingSendToMainEndpoint  *bool               `yaml:"profiling_send_to_main_endpoint"` // default true
}

// ProcessConfig mirrors the datadog.yaml `process_config:` block (only the keys
// the micro-agent honours). The rest are ignored.
type ProcessConfig struct {
	ProcessDDURL        string                  `yaml:"process_dd_url"`
	ProcessCollection   ProcessCollectionConfig `yaml:"process_collection"`
	AdditionalEndpoints map[string][]string     `yaml:"additional_endpoints"` // dual-shipping (url -> keys)
}

// ProcessCollectionConfig is `process_config.process_collection:`.
type ProcessCollectionConfig struct {
	Enabled bool `yaml:"enabled"`
}

// LogsAgentConfig is the datadog.yaml `logs_config:` block, settings for the
// logs agent as a whole, distinct from the per-source `logs:` entries.
type LogsAgentConfig struct {
	LogsDDURL           string         `yaml:"logs_dd_url"`
	BatchWait           int            `yaml:"batch_wait"` // seconds
	BatchMaxSize        int            `yaml:"batch_max_size"`
	BatchMaxContentSize int            `yaml:"batch_max_content_size"` // bytes, pre-compression
	AdditionalEndpoints []LogsEndpoint `yaml:"additional_endpoints"`   // dual-shipping: extra logs intakes
}

// LogsEndpoint is one entry of logs_config.additional_endpoints. The micro agent is
// HTTP-only, so it reads the HTTP-relevant fields. is_reliable defaults to true,
// matching the stock Agent, and here sets only how loudly a failed delivery is
// logged: the primary endpoint alone gates the registry offset (see internal/logs).
type LogsEndpoint struct {
	APIKey     string `yaml:"api_key" json:"api_key"`
	Host       string `yaml:"host" json:"host"`
	Port       int    `yaml:"port" json:"port"`
	IsReliable *bool  `yaml:"is_reliable" json:"is_reliable"`
	UseSSL     *bool  `yaml:"use_ssl" json:"use_ssl"`
}

// reliable reports the endpoint's is_reliable value, defaulting to true.
func (e LogsEndpoint) reliable() bool { return e.IsReliable == nil || *e.IsReliable }

// url builds the endpoint's logs intake URL: https://host:port/api/v2/logs, http
// when use_ssl is false. Port 0 is omitted (the scheme's default applies).
func (e LogsEndpoint) url() string {
	scheme := "https"
	if e.UseSSL != nil && !*e.UseSSL {
		scheme = "http"
	}
	host := e.Host
	if e.Port != 0 {
		host = net.JoinHostPort(e.Host, strconv.Itoa(e.Port))
	}
	return scheme + "://" + host + "/api/v2/logs"
}

// Proxy mirrors the datadog.yaml `proxy:` block. Only the https proxy is read:
// every intake the agent talks to is HTTPS.
type Proxy struct {
	HTTPS string `yaml:"https"`
}

// Default returns a Config populated with the same defaults as the stock Agent.
// Unmarshaling datadog.yaml on top of this preserves any key the file omits.
func Default() *Config {
	return &Config{
		Site:                     "datadoghq.com",
		DogstatsdPort:            8125,
		ConfdPath:                "/etc/datadog-agent/conf.d",
		RunPath:                  "/opt/datadog-agent/run",
		EnableMetadataCollection: true,
		LogsConfig: LogsAgentConfig{
			BatchWait:           5,
			BatchMaxSize:        1000,
			BatchMaxContentSize: 5_000_000,
		},
		APMConfig: APMConfig{ReceiverHost: "localhost", ReceiverPort: 8126},
	}
}

// Load reads datadog.yaml from path, applies it over the defaults, then applies
// DD_* environment overrides. A missing file is not an error: the agent can run
// from environment variables alone, as it commonly does in containers.
func Load(path string) (*Config, error) {
	cfg := Default()
	data, err := os.ReadFile(path)
	switch {
	case err == nil:
		if err := yaml.Unmarshal(data, cfg); err != nil {
			return nil, fmt.Errorf("parse %s: %w", path, err)
		}
	case errors.Is(err, fs.ErrNotExist):
		// fall through: defaults + environment
	default:
		return nil, err
	}
	cfg.applyEnv()
	return cfg, nil
}

// Validate reports whether the configuration is usable.
func (c *Config) Validate() error {
	if c.APIKey == "" {
		return errors.New("api_key is required (set it in datadog.yaml or DD_API_KEY)")
	}
	return nil
}

// infraBase is the infra intake base URL: dd_url if set, else the agent-version-
// prefixed host for <site>, e.g. https://0-1-0-app.agent.datadoghq.eu. The stock
// Agent derives this with AddAgentVersionToDomain (pkg/config/utils/endpoints.go):
// the infra intake (both /api/v1/series and the v5 /intake/ host metadata) is
// served by the "<version>-app.agent.<site>" host, not "app.<site>". (app.<site>
// happens to accept /api/v1/series, but its /intake/ returns 202 and silently drops
// the payload, so the host never gets its OS/agent/gohai metadata.) A dd_url goes
// through the same rule as an additional endpoint: a well-known app.<site> is
// version-prefixed, exactly as the stock Agent prefixes its main domain, and a
// custom proxy URL is used verbatim.
func (c *Config) infraBase() string {
	if c.DDURL != "" {
		return infraDomain(c.DDURL)
	}
	return "https://" + agentDomainPrefix() + "-app.agent." + c.Site
}

// agentDomainPrefix renders the agent version as "<maj>-<min>-<patch>", matching the
// stock Agent's getDomainPrefix (pkg/config/utils/endpoints.go).
func agentDomainPrefix() string {
	v := intake.Version
	if i := strings.IndexAny(v, "-+"); i >= 0 {
		v = v[:i] // drop any pre-release/build suffix
	}
	p := strings.SplitN(v, ".", 4)
	for len(p) < 3 {
		p = append(p, "0")
	}
	return p[0] + "-" + p[1] + "-" + p[2]
}

// The infra intake endpoints. Each returns the primary destination (infraBase plus
// the route, keyed by api_key) followed by every additional_endpoints destination,
// so a single config dual-ships series, checks, events, and host metadata to several
// orgs at once. The primary is always first.

// MetricsEndpoints is the series intake.
func (c *Config) MetricsEndpoints() []intake.Endpoint { return c.infraEndpoints("/api/v1/series") }

// CheckRunEndpoints is the service-check intake (a JSON array of checks), on the
// same infra intake as the series.
func (c *Config) CheckRunEndpoints() []intake.Endpoint { return c.infraEndpoints("/api/v1/check_run") }

// IntakeEndpoints is the host-metadata ("v5") intake that populates the
// Infrastructure List.
func (c *Config) IntakeEndpoints() []intake.Endpoint { return c.infraEndpoints("/intake/") }

// EventsEndpoints is the events intake: the legacy v5 /intake/ endpoint (same as
// host metadata). The public /api/v1/events is not used. It rejects gzipped bodies,
// which the intake client always sends.
func (c *Config) EventsEndpoints() []intake.Endpoint { return c.IntakeEndpoints() }

// MetadataEndpoints is the modern inventory metadata endpoint (inventory_host and
// inventory_agent), which populates the host-detail page in the Infrastructure UI.
func (c *Config) MetadataEndpoints() []intake.Endpoint { return c.infraEndpoints("/api/v1/metadata") }

// infraEndpoints builds the primary plus additional infra destinations for one route.
func (c *Config) infraEndpoints(route string) []intake.Endpoint {
	eps := []intake.Endpoint{{URL: c.infraBase() + route, APIKey: c.APIKey, Reliable: true}}
	return appendAdditional(eps, c.AdditionalEndpoints, func(domain string) string {
		return infraDomain(domain) + route
	})
}

// ddURLRegexp matches the well-known Datadog infra domains (app.<site>, with an
// optional mrf failover label and an optional datacenter label like us3), the same
// set the stock Agent version-prefixes (pkg/config/utils/endpoints.go).
var ddURLRegexp = regexp.MustCompile(`^app(\.mrf)?\.([a-z]{2,}\d{1,2}\.)?(datad(oghq|0g)\.(com|eu)|ddog-gov\.com)\.?$`)

// infraDomain turns a configured additional_endpoints domain into its real intake
// host, mirroring the stock AddAgentVersionToDomain: a Datadog domain (app.<site>)
// is version-prefixed to <maj>-<min>-<patch>-app.agent.<site>, so it lands on the
// same host the primary does. A custom proxy URL is returned unchanged.
func infraDomain(raw string) string {
	raw = strings.TrimRight(raw, "/")
	u, err := url.Parse(raw)
	if err != nil || !ddURLRegexp.MatchString(u.Host) {
		return raw
	}
	_, rest, _ := strings.Cut(u.Host, ".") // drop the leading "app" label
	u.Host = agentDomainPrefix() + "-app.agent." + rest
	return strings.TrimRight(u.String(), "/")
}

// appendAdditional adds one endpoint per (domain, key) in extra, resolving each
// domain with urlFor. A (url, key) pair already present, whether from an earlier
// additional entry or from the primary it version-prefixes onto (an additional
// app.<site> resolves to the same host as the primary), is dropped, so the agent
// never double-ships to one org with one key. Domains are sorted for stable output.
func appendAdditional(eps []intake.Endpoint, extra map[string][]string, urlFor func(domain string) string) []intake.Endpoint {
	seen := make(map[string]bool, len(eps))
	for _, e := range eps {
		seen[e.URL+"\x00"+e.APIKey] = true
	}
	for _, domain := range slices.Sorted(maps.Keys(extra)) { // sorted for stable output
		u := urlFor(domain)
		for _, key := range extra[domain] {
			if key = strings.TrimSpace(key); key == "" {
				continue
			}
			id := u + "\x00" + key
			if seen[id] {
				continue
			}
			seen[id] = true
			eps = append(eps, intake.Endpoint{URL: u, APIKey: key, Reliable: true})
		}
	}
	return eps
}

// LogsEndpoints is the logs intake: the primary (LogsURL) followed by every
// logs_config.additional_endpoints destination, each keyed by its own api_key.
func (c *Config) LogsEndpoints() []intake.Endpoint {
	primary := intake.Endpoint{URL: c.LogsURL(), APIKey: c.APIKey, Reliable: true}
	eps := []intake.Endpoint{primary}
	seen := map[string]bool{primary.URL + "\x00" + primary.APIKey: true}
	for _, e := range c.LogsConfig.AdditionalEndpoints {
		key := strings.TrimSpace(e.APIKey)
		if key == "" || e.Host == "" {
			continue
		}
		u := e.url()
		id := u + "\x00" + key
		if seen[id] {
			continue
		}
		seen[id] = true
		eps = append(eps, intake.Endpoint{URL: u, APIKey: key, Reliable: e.reliable()})
	}
	return eps
}

// LogsURL is the logs intake endpoint: logs_dd_url if set, else the HTTP intake
// for <site>. A logs_dd_url without a scheme is assumed to be HTTPS.
func (c *Config) LogsURL() string {
	base := c.LogsConfig.LogsDDURL
	switch {
	case base == "":
		base = "https://agent-http-intake.logs." + c.Site
	case !strings.Contains(base, "://"):
		base = "https://" + base
	}
	return strings.TrimRight(base, "/") + "/api/v2/logs"
}

// ProcessURL is the process ("Live Processes") intake endpoint. Unlike the infra
// intake it is not agent-version-prefixed: the stock Agent posts to
// "https://process.<site>" (process_config.process_dd_url overrides).
func (c *Config) ProcessURL() string {
	base := c.ProcessConfig.ProcessDDURL
	if base == "" {
		base = "https://process." + c.Site
	}
	return strings.TrimRight(base, "/") + "/api/v1/collector"
}

// ProcessEndpoints is the process intake: the primary (ProcessURL) plus every
// process_config.additional_endpoints destination, each keyed by its own api_key.
// Like the primary, additional process domains are not version-prefixed.
func (c *Config) ProcessEndpoints() []intake.Endpoint {
	eps := []intake.Endpoint{{URL: c.ProcessURL(), APIKey: c.APIKey, Reliable: true}}
	return appendAdditional(eps, c.ProcessConfig.AdditionalEndpoints, func(domain string) string {
		return strings.TrimRight(domain, "/") + "/api/v1/collector"
	})
}

// ProcessEnabled reports whether Live Processes collection is on.
func (c *Config) ProcessEnabled() bool { return c.ProcessConfig.ProcessCollection.Enabled }

// ProfilingURL is where the profiling proxy forwards uploads. The stock Agent
// uses profiling_dd_url verbatim when set, otherwise intake.profile.<site>. Like
// the process intake it is not agent-version-prefixed.
func (c *Config) ProfilingURL() string {
	if u := c.APMConfig.ProfilingDDURL; u != "" {
		return u
	}
	return "https://intake.profile." + c.Site + "/api/v2/profile"
}

// ProfilingEndpoints is where the profiling proxy fans uploads out to: the main
// endpoint (ProfilingURL, unless profiling_send_to_main_endpoint is false) plus
// every profiling_additional_endpoints destination. Unlike the infra and logs
// additional endpoints, these are full URLs (they include /api/v2/profile), as the
// stock trace-agent expects.
func (c *Config) ProfilingEndpoints() []intake.Endpoint {
	var eps []intake.Endpoint
	if c.APMConfig.ProfilingSendToMainEndpoint == nil || *c.APMConfig.ProfilingSendToMainEndpoint {
		eps = append(eps, intake.Endpoint{URL: c.ProfilingURL(), APIKey: c.APIKey, Reliable: true})
	}
	return appendAdditional(eps, c.APMConfig.ProfilingAdditionalEndpoints, func(domain string) string {
		return strings.TrimRight(domain, "/")
	})
}

// APMEnabled reports whether the profiling proxy should run.
func (c *Config) APMEnabled() bool { return c.APMConfig.Enabled }

// DefaultEnv is the env the proxy reports in X-Datadog-Additional-Tags. The stock
// trace-agent defaults an unset env to "none".
func (c *Config) DefaultEnv() string {
	if c.Env == "" {
		return "none"
	}
	return c.Env
}

func (c *Config) applyEnv() {
	envString("DD_API_KEY", &c.APIKey)
	envString("DD_SITE", &c.Site)
	envString("DD_URL", &c.DDURL) // the stock Agent accepts both names for dd_url
	envString("DD_DD_URL", &c.DDURL)
	envJSON("DD_ADDITIONAL_ENDPOINTS", &c.AdditionalEndpoints)
	envJSON("DD_LOGS_CONFIG_ADDITIONAL_ENDPOINTS", &c.LogsConfig.AdditionalEndpoints)
	envString("DD_ENV", &c.Env)
	envString("DD_HOSTNAME", &c.Hostname)
	envString("DD_HOSTNAME_FILE", &c.HostnameFile)
	envList("DD_TAGS", &c.Tags)
	envInt("DD_DOGSTATSD_PORT", &c.DogstatsdPort)
	envString("DD_DOGSTATSD_SOCKET", &c.DogstatsdSocket)
	envBool("DD_DOGSTATSD_NON_LOCAL_TRAFFIC", &c.DogstatsdNonLocalTraffic)
	envBool("DD_LOGS_ENABLED", &c.LogsEnabled)
	envString("DD_LOGS_CONFIG_LOGS_DD_URL", &c.LogsConfig.LogsDDURL)
	envInt("DD_LOGS_CONFIG_BATCH_WAIT", &c.LogsConfig.BatchWait)
	envInt("DD_LOGS_CONFIG_BATCH_MAX_SIZE", &c.LogsConfig.BatchMaxSize)
	envInt("DD_LOGS_CONFIG_BATCH_MAX_CONTENT_SIZE", &c.LogsConfig.BatchMaxContentSize)
	envBool("DD_SKIP_SSL_VALIDATION", &c.SkipSSLValidation)
	envBool("DD_ENABLE_METADATA_COLLECTION", &c.EnableMetadataCollection)
	envString("DD_VENTI_URL", &c.VentiURL)
	envBool("DD_PROCESS_CONFIG_PROCESS_COLLECTION_ENABLED", &c.ProcessConfig.ProcessCollection.Enabled)
	envString("DD_PROCESS_CONFIG_PROCESS_DD_URL", &c.ProcessConfig.ProcessDDURL)
	envJSON("DD_PROCESS_CONFIG_ADDITIONAL_ENDPOINTS", &c.ProcessConfig.AdditionalEndpoints)
	envBool("DD_APM_ENABLED", &c.APMConfig.Enabled)
	envInt("DD_APM_RECEIVER_PORT", &c.APMConfig.ReceiverPort)
	envBool("DD_APM_NON_LOCAL_TRAFFIC", &c.APMConfig.NonLocalTraffic)
	envString("DD_APM_PROFILING_DD_URL", &c.APMConfig.ProfilingDDURL)
	envJSON("DD_APM_PROFILING_ADDITIONAL_ENDPOINTS", &c.APMConfig.ProfilingAdditionalEndpoints)
	envBoolPtr("DD_APM_PROFILING_SEND_TO_MAIN_ENDPOINT", &c.APMConfig.ProfilingSendToMainEndpoint)
	envString("DD_CONFD_PATH", &c.ConfdPath)
	envString("DD_RUN_PATH", &c.RunPath)
	envString("DD_PROXY_HTTPS", &c.Proxy.HTTPS)
}

func envString(key string, dst *string) {
	if v, ok := os.LookupEnv(key); ok {
		*dst = v
	}
}

func envBool(key string, dst *bool) {
	if v, ok := os.LookupEnv(key); ok {
		if b, err := strconv.ParseBool(v); err == nil {
			*dst = b
		}
	}
}

func envInt(key string, dst *int) {
	if v, ok := os.LookupEnv(key); ok {
		if n, err := strconv.Atoi(v); err == nil {
			*dst = n
		}
	}
}

// envBoolPtr is envBool for optional (pointer) booleans, whose nil means "unset".
func envBoolPtr(key string, dst **bool) {
	if v, ok := os.LookupEnv(key); ok {
		if b, err := strconv.ParseBool(v); err == nil {
			*dst = &b
		}
	}
}

// envJSON parses a JSON-valued env var over dst, the form the stock Agent uses for
// the additional_endpoints maps and lists (e.g. DD_ADDITIONAL_ENDPOINTS=
// '{"https://app.datadoghq.eu":["key2"]}'). The env value replaces the file value
// outright, decoding into a fresh value first: json.Unmarshal into a populated map
// merges instead of replacing. A malformed value is ignored, leaving the
// file-provided value in place, the same tolerance the rest of the loader has.
func envJSON[T any](key string, dst *T) {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		var fresh T
		if json.Unmarshal([]byte(v), &fresh) == nil {
			*dst = fresh
		}
	}
}

// envList splits on whitespace and commas, matching how DD_TAGS is written.
func envList(key string, dst *[]string) {
	if v, ok := os.LookupEnv(key); ok {
		*dst = strings.FieldsFunc(v, func(r rune) bool {
			return r == ',' || r == ' ' || r == '\t' || r == '\n'
		})
	}
}

// LogSource is one entry of a `logs:` array, as found in conf.d integration
// configs. Two types are supported: "file" (Path is the file or glob to tail) and
// "journald" (the systemd journal, read on Linux only, see internal/logs).
type LogSource struct {
	Type               string           `yaml:"type"`
	Path               string           `yaml:"path"`
	Service            string           `yaml:"service"`
	Source             string           `yaml:"source"`
	Tags               []string         `yaml:"tags"`
	LogProcessingRules []ProcessingRule `yaml:"log_processing_rules"`

	// journald sources only. Path doubles as the journal directory when set, else
	// the system journal is read. container_mode and process_raw_message are
	// intentionally unsupported: the micro agent has no container tagger, and it
	// always ships the structured body the stock Agent sends by default.
	StartPosition    string   `yaml:"start_position"` // beginning, end (default), forceBeginning, forceEnd
	IncludeUnits     []string `yaml:"include_units"`
	ExcludeUnits     []string `yaml:"exclude_units"`
	IncludeUserUnits []string `yaml:"include_user_units"`
	ExcludeUserUnits []string `yaml:"exclude_user_units"`
	IncludeMatches   []string `yaml:"include_matches"`
	ExcludeMatches   []string `yaml:"exclude_matches"`
}

// ProcessingRule is one entry in a source's log_processing_rules (the stock Agent
// schema). Type is one of multi_line, mask_sequences, exclude_at_match,
// include_at_match. Pattern is a regular expression. ReplacePlaceholder is the
// replacement text for mask_sequences.
type ProcessingRule struct {
	Type               string `yaml:"type"`
	Name               string `yaml:"name"`
	Pattern            string `yaml:"pattern"`
	ReplacePlaceholder string `yaml:"replace_placeholder"`
}

// LoadLogSources scans confPath for integration configs and returns every file
// and journald log source declared in their `logs:` blocks. It reads
// <confPath>/*.d/conf.yaml and <confPath>/*.yaml. Sources of other types are
// skipped and named in skipped so the caller can log them. A missing confPath
// yields no sources and no error.
func LoadLogSources(confPath string) (sources []LogSource, skipped []string, err error) {
	patterns := []string{
		filepath.Join(confPath, "*.d", "conf.yaml"),
		filepath.Join(confPath, "*.yaml"),
	}
	var files []string
	for _, p := range patterns {
		matches, err := filepath.Glob(p)
		if err != nil {
			return nil, nil, err
		}
		files = append(files, matches...)
	}

	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			return nil, nil, err
		}
		var doc struct {
			Logs []LogSource `yaml:"logs"`
		}
		if err := yaml.Unmarshal(data, &doc); err != nil {
			return nil, nil, fmt.Errorf("parse %s: %w", f, err)
		}
		for _, s := range doc.Logs {
			switch s.Type {
			case "file", "journald":
				sources = append(sources, s)
			default:
				skipped = append(skipped, fmt.Sprintf("%s: type %q", f, s.Type))
			}
		}
	}
	return sources, skipped, nil
}
