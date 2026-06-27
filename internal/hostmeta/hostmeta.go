// Package hostmeta reports host metadata to Datadog's infra-host intake (the
// legacy "v5" payload), which is what makes a host appear in the Infrastructure
// List with its OS, agent version, tags, and a host detail page. It reuses the
// shared intake transport. Only the URL (/intake/) and body are specific here.
package hostmeta

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"net"
	"os"
	"runtime"
	"strings"
	"time"

	"github.com/0intro/dd-micro-agent/internal/intake"
)

// payload is the v5 host metadata payload. Field names match the stock Agent so
// the backend recognizes it.
type payload struct {
	APIKey           string      `json:"apiKey"`
	AgentVersion     string      `json:"agentVersion"`
	UUID             string      `json:"uuid"`
	InternalHostname string      `json:"internalHostname"`
	OS               string      `json:"os"`
	AgentFlavor      string      `json:"agent-flavor"`
	Python           string      `json:"python"`
	SystemStats      systemStats `json:"systemStats"`
	Meta             meta        `json:"meta"`
	HostTags         hostTags    `json:"host-tags"`
	Gohai            string      `json:"gohai"` // JSON-encoded gohai (a string field, as the backend expects)
}

// systemStats is the legacy v5 block the backend turns into the host's meta
// fields. Its `platform` drives the OS icon in the Infrastructure List, and the
// per-OS version arrays (only one is populated) tell Datadog which OS it is.
type systemStats struct {
	CPUCores  int       `json:"cpuCores"`
	Machine   string    `json:"machine"`
	Platform  string    `json:"platform"`
	PythonV   string    `json:"pythonV"`
	Processor string    `json:"processor"`
	MacV      osVersion `json:"macV"`
	NixV      osVersion `json:"nixV"`
	FbsdV     osVersion `json:"fbsdV"`
	WinV      osVersion `json:"winV"`
}

// osVersion is one legacy v5 version array. The stock shapes differ per OS
// (three flat strings for nixV, a nested empty three-string array in macV's
// middle element on darwin, two strings on Windows), so the elements are any.
type osVersion []any

type meta struct {
	Hostname       string   `json:"hostname"`
	SocketHostname string   `json:"socket-hostname"`
	HostAliases    []string `json:"host_aliases"`
	Timezones      []string `json:"timezones"`
}

type hostTags struct {
	System []string `json:"system"`
}

// gohai mirrors the stock gohai payload (minus processes). It is marshaled to a
// string and embedded in payload.Gohai. Numeric values go out as JSON numbers
// where stock gohai stringifies them with unit suffixes, a divergence the
// intake tolerates (verified live).
type gohai struct {
	CPU        gohaiCPU      `json:"cpu"`
	FileSystem []gohaiFS     `json:"filesystem"`
	Memory     gohaiMemory   `json:"memory"`
	Network    gohaiNetwork  `json:"network"`
	Platform   gohaiPlatform `json:"platform"`
}

type gohaiPlatform struct {
	GoVersion     string `json:"goV"`
	GoOS          string `json:"GOOS"`
	GoArch        string `json:"GOOARCH"`
	KernelName    string `json:"kernel_name"`
	KernelRelease string `json:"kernel_release"`
	Hostname      string `json:"hostname"`
	Machine       string `json:"machine"`
	OS            string `json:"os"`
	KernelVersion string `json:"kernel_version"`
	Processor     string `json:"processor"`
}

type gohaiCPU struct {
	VendorID             string  `json:"vendor_id"`
	ModelName            string  `json:"model_name"`
	CPUCores             uint64  `json:"cpu_cores"`
	CPULogicalProcessors uint64  `json:"cpu_logical_processors"`
	Mhz                  float64 `json:"mhz"`
	Family               string  `json:"family"`
	Model                string  `json:"model"`
	Stepping             string  `json:"stepping"`
}

type gohaiMemory struct {
	Total     uint64 `json:"total"`      // bytes
	SwapTotal uint64 `json:"swap_total"` // kB
}

type gohaiFS struct {
	Name      string `json:"name"`
	SizeKB    uint64 `json:"kb_size"`
	MountedOn string `json:"mounted_on"`
}

type gohaiNetwork struct {
	MacAddress  string       `json:"macaddress"`
	IPAddress   string       `json:"ipaddress"`
	IPAddressV6 string       `json:"ipaddressv6"`
	Interfaces  []gohaiIface `json:"interfaces"`
}

type gohaiIface struct {
	Name       string   `json:"name"`
	MacAddress string   `json:"macaddress"`
	IPv4       []string `json:"ipv4"`
	IPv6       []string `json:"ipv6"`
}

// Reporter periodically sends the host metadata payload.
type Reporter struct {
	client            *intake.Client
	intakeEndpoints   []intake.Endpoint // /intake/ (legacy v5 host metadata)
	metadataEndpoints []intake.Endpoint // /api/v1/metadata (modern inventory)
	hostname          string
	tags              []string
	interval          time.Duration
	log               *slog.Logger
}

// Options configures a Reporter.
type Options struct {
	Client            *intake.Client
	IntakeEndpoints   []intake.Endpoint // the /intake/ endpoints (legacy v5 host metadata)
	MetadataEndpoints []intake.Endpoint // the /api/v1/metadata endpoints (modern inventory metadata)
	Hostname          string
	Tags              []string
	Interval          time.Duration // defaults to 30m
	Logger            *slog.Logger
}

// New returns a Reporter.
func New(o Options) *Reporter {
	if o.Interval == 0 {
		o.Interval = 30 * time.Minute
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	return &Reporter{
		client:            o.Client,
		intakeEndpoints:   o.IntakeEndpoints,
		metadataEndpoints: o.MetadataEndpoints,
		hostname:          o.Hostname,
		tags:              o.Tags,
		interval:          o.Interval,
		log:               o.Logger,
	}
}

// Run sends the payload once at startup so the host appears promptly, then on the
// interval until ctx is cancelled.
func (r *Reporter) Run(ctx context.Context) {
	r.send(ctx)
	ticker := time.NewTicker(r.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			r.send(ctx)
		}
	}
}

func (r *Reporter) send(ctx context.Context) {
	g := collectGohai(r.hostname)
	s := systemStatsFrom(g)
	nowNs := time.Now().UnixNano()

	// One bounded round, derived from Run's ctx so shutdown cancels an
	// in-flight post.
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	// Legacy v5 host metadata -> /intake/ (lands the host in the Infrastructure List).
	// The envelope embeds the API key, so each org gets a body built with its own.
	if _, err := r.client.PostAllFunc(ctx, r.intakeEndpoints, func(apiKey string) ([]byte, error) {
		return r.buildV5(g, s, apiKey)
	}); err != nil {
		r.log.Warn("host metadata send failed", "err", err)
	} else {
		r.log.Debug("host metadata sent", "hostname", r.hostname)
	}

	// Modern inventory metadata -> /api/v1/metadata (populates the host-detail page,
	// i.e. the "host" and "Metrics" tabs). The body carries no key.
	if len(r.metadataEndpoints) > 0 {
		if body, err := json.Marshal(inventoryHost(r.hostname, g, s, nowNs)); err == nil {
			if _, err := r.client.PostAll(ctx, r.metadataEndpoints, body); err != nil {
				r.log.Warn("inventory host metadata send failed", "err", err)
			}
		}
		if body, err := json.Marshal(inventoryAgent(r.hostname, nowNs)); err == nil {
			if _, err := r.client.PostAll(ctx, r.metadataEndpoints, body); err != nil {
				r.log.Warn("inventory agent metadata send failed", "err", err)
			}
		}
	}
}

// build returns the legacy v5 payload for one API key. It collects the host facts
// itself (used by tests). The send path uses buildV5 directly to share one
// collection with inventory.
func (r *Reporter) build(apiKey string) ([]byte, error) {
	g := collectGohai(r.hostname)
	return r.buildV5(g, systemStatsFrom(g), apiKey)
}

func (r *Reporter) buildV5(g gohai, s systemStats, apiKey string) ([]byte, error) {
	gj, err := json.Marshal(g)
	if err != nil {
		return nil, err
	}
	socket, _ := os.Hostname()
	tz, _ := time.Now().Zone()
	tags := r.tags
	if tags == nil {
		tags = []string{}
	}
	return json.Marshal(payload{
		APIKey:           apiKey,
		AgentVersion:     intake.Version,
		UUID:             hostUUID(r.hostname),
		InternalHostname: r.hostname,
		OS:               osField(),
		AgentFlavor:      "agent",
		SystemStats:      s,
		Meta: meta{
			Hostname:       r.hostname,
			SocketHostname: socket,
			HostAliases:    []string{},
			Timezones:      []string{tz},
		},
		HostTags: hostTags{System: tags},
		Gohai:    string(gj),
	})
}

// systemStatsFrom builds the systemStats block. Platform is the Go OS name. The
// matching per-OS version array (nixV/macV/winV) is filled by setOSVersion, which
// is what makes the backend show the right OS and icon.
func systemStatsFrom(g gohai) systemStats {
	s := systemStats{
		CPUCores:  int(g.CPU.CPUCores),
		Machine:   runtime.GOARCH,
		Platform:  runtime.GOOS,
		Processor: g.CPU.ModelName,
		MacV:      osVersion{"", "", ""},
		NixV:      osVersion{"", "", ""},
		FbsdV:     osVersion{"", "", ""},
		WinV:      osVersion{"", "", ""},
	}
	setOSVersion(&s)
	return s
}

// osField is the top-level "os" value the backend keys the OS icon on: "win32" on
// Windows (its in-app icon name), the Go OS name otherwise.
func osField() string {
	if runtime.GOOS == "windows" {
		return "win32"
	}
	return runtime.GOOS
}

// hostUUID returns a stable UUID-formatted identifier for the reported host. It
// hashes the machine id (when available) together with the hostname, so it is
// stable across restarts, anchored to the machine, yet distinct per hostname.
func hostUUID(hostname string) string {
	seed := hostname
	if b, err := os.ReadFile("/etc/machine-id"); err == nil {
		seed = strings.TrimSpace(string(b)) + "/" + hostname
	}
	sum := sha256.Sum256([]byte(seed))
	h := hex.EncodeToString(sum[:16])
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}

// collectNetwork gathers interface metadata via the standard library, so it works
// on every platform. Loopback and link-local addresses are skipped.
func collectNetwork() gohaiNetwork {
	n := gohaiNetwork{Interfaces: []gohaiIface{}}
	ifaces, err := net.Interfaces()
	if err != nil {
		return n
	}
	for _, ifc := range ifaces {
		if ifc.Flags&net.FlagLoopback != 0 {
			continue
		}
		gi := gohaiIface{Name: ifc.Name, MacAddress: ifc.HardwareAddr.String(), IPv4: []string{}, IPv6: []string{}}
		addrs, _ := ifc.Addrs()
		for _, a := range addrs {
			ipnet, ok := a.(*net.IPNet)
			if !ok || ipnet.IP.IsLinkLocalUnicast() || ipnet.IP.IsLinkLocalMulticast() {
				continue
			}
			if v4 := ipnet.IP.To4(); v4 != nil {
				gi.IPv4 = append(gi.IPv4, v4.String())
				if n.IPAddress == "" {
					n.IPAddress, n.MacAddress = v4.String(), gi.MacAddress
				}
			} else {
				gi.IPv6 = append(gi.IPv6, ipnet.IP.String())
				if n.IPAddressV6 == "" {
					n.IPAddressV6 = ipnet.IP.String()
				}
			}
		}
		n.Interfaces = append(n.Interfaces, gi)
	}
	return n
}
