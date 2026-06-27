package hostmeta

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"testing"

	"github.com/0intro/dd-micro-agent/internal/intake"
)

func TestHostUUIDStable(t *testing.T) {
	a, b := hostUUID("host-a"), hostUUID("host-a")
	if a != b {
		t.Errorf("uuid not stable: %q != %q", a, b)
	}
	if hostUUID("host-a") == hostUUID("host-b") {
		t.Error("different hostnames produced the same uuid")
	}
	if len(a) != 36 || a[8] != '-' || a[13] != '-' || a[18] != '-' || a[23] != '-' {
		t.Errorf("uuid not in 8-4-4-4-12 form: %q", a)
	}
}

func TestBuildPayload(t *testing.T) {
	r := New(Options{Hostname: "test-host", Tags: []string{"env:test"}})
	body, err := r.build("k")
	if err != nil {
		t.Fatal(err)
	}

	var p struct {
		InternalHostname string `json:"internalHostname"`
		AgentVersion     string `json:"agentVersion"`
		OS               string `json:"os"`
		Meta             struct {
			Hostname  string
			Timezones []string
		} `json:"meta"`
		HostTags struct {
			System []string `json:"system"`
		} `json:"host-tags"`
		Gohai string `json:"gohai"`
	}
	if err := json.Unmarshal(body, &p); err != nil {
		t.Fatal(err)
	}
	if p.InternalHostname != "test-host" {
		t.Errorf("internalHostname = %q", p.InternalHostname)
	}
	if p.AgentVersion != intake.Version || p.OS == "" {
		t.Errorf("agentVersion/os = %q/%q", p.AgentVersion, p.OS)
	}
	if len(p.HostTags.System) != 1 || p.HostTags.System[0] != "env:test" {
		t.Errorf("host-tags = %v", p.HostTags.System)
	}
	if len(p.Meta.Timezones) != 1 || p.Meta.Timezones[0] == "" {
		t.Errorf("timezones = %v, want the local zone abbreviation", p.Meta.Timezones)
	}

	// gohai is an embedded JSON string. It must parse and carry the hostname.
	var g struct {
		Platform struct{ Hostname string } `json:"platform"`
	}
	if err := json.Unmarshal([]byte(p.Gohai), &g); err != nil {
		t.Fatalf("gohai is not valid JSON: %v", err)
	}
	if g.Platform.Hostname != "test-host" {
		t.Errorf("gohai platform.hostname = %q", g.Platform.Hostname)
	}
}

// TestBuildPayloadOSIcon locks the fields that drive the Infrastructure List OS
// icon, matching the stock Agent: top-level os ("win32" on Windows, else GOOS),
// platform (GOOS), an empty fbsdV (FreeBSD reports via nixV), and (the bug that
// hid the FreeBSD logo) a lowercase platform token in nixV[0].
func TestBuildPayloadOSIcon(t *testing.T) {
	body, err := New(Options{Hostname: "h"}).build("k")
	if err != nil {
		t.Fatal(err)
	}
	var p struct {
		OS          string `json:"os"`
		SystemStats struct {
			Platform string `json:"platform"`
			MacV     []any  `json:"macV"`
			NixV     []any  `json:"nixV"`
			FbsdV    []any  `json:"fbsdV"`
			WinV     []any  `json:"winV"`
		} `json:"systemStats"`
	}
	if err := json.Unmarshal(body, &p); err != nil {
		t.Fatal(err)
	}

	wantOS := runtime.GOOS
	if runtime.GOOS == "windows" {
		wantOS = "win32" // the stock Agent's in-app icon name for Windows
	}
	if p.OS != wantOS {
		t.Errorf("os = %q, want %q", p.OS, wantOS)
	}
	if p.SystemStats.Platform != runtime.GOOS {
		t.Errorf("systemStats.platform = %q, want %q", p.SystemStats.Platform, runtime.GOOS)
	}
	if p.SystemStats.FbsdV[0] != "" {
		t.Errorf("fbsdV must be empty (FreeBSD uses nixV), got %v", p.SystemStats.FbsdV)
	}

	// Exactly one version array is populated, per OS family, in the stock shape.
	switch runtime.GOOS {
	case "darwin":
		// macV = [version, ["","",""], arch]: the middle element is a nested
		// empty three-string array, not a flat string.
		if len(p.SystemStats.MacV) != 3 {
			t.Fatalf("macV = %v, want 3 elements", p.SystemStats.MacV)
		}
		if v, _ := p.SystemStats.MacV[0].(string); v == "" {
			t.Errorf("macV[0] empty on darwin: %v", p.SystemStats.MacV)
		}
		if mid, ok := p.SystemStats.MacV[1].([]any); !ok || len(mid) != 3 {
			t.Errorf("macV[1] = %v, want a nested empty three-string array", p.SystemStats.MacV[1])
		}
		if a, _ := p.SystemStats.MacV[2].(string); a != runtime.GOARCH {
			t.Errorf("macV[2] = %v, want %q", p.SystemStats.MacV[2], runtime.GOARCH)
		}
	case "windows":
		// winV = [product, build]: two elements, like the stock's [2]string.
		if len(p.SystemStats.WinV) != 2 {
			t.Fatalf("winV = %v, want 2 elements", p.SystemStats.WinV)
		}
		if v, _ := p.SystemStats.WinV[0].(string); v == "" {
			t.Errorf("winV[0] empty on windows: %v", p.SystemStats.WinV)
		}
	default: // linux + freebsd/openbsd/netbsd report via nixV
		if len(p.SystemStats.NixV) != 3 {
			t.Fatalf("nixV = %v, want 3 elements", p.SystemStats.NixV)
		}
		got, _ := p.SystemStats.NixV[0].(string)
		if got == "" {
			t.Errorf("nixV[0] empty: %v", p.SystemStats.NixV)
		}
		if got != strings.ToLower(got) {
			t.Errorf("nixV[0] = %q must be lowercase or the backend shows no OS icon", got)
		}
	}
}

// TestOSVersionShapes pins the version-array wire bytes to the stock v5 shapes:
// nixV three flat strings, darwin's macV a nested empty three-string array in
// its middle element, Windows' winV two strings.
func TestOSVersionShapes(t *testing.T) {
	for _, tt := range []struct {
		name string
		v    osVersion
		want string
	}{
		{"empty", osVersion{"", "", ""}, `["","",""]`},
		{"nix", osVersion{"freebsd", "14.1", ""}, `["freebsd","14.1",""]`},
		{"darwin", osVersion{"15.5", [3]string{"", "", ""}, "arm64"}, `["15.5",["","",""],"arm64"]`},
		{"windows", osVersion{"Windows 10 Pro", "19045"}, `["Windows 10 Pro","19045"]`},
	} {
		b, err := json.Marshal(tt.v)
		if err != nil {
			t.Fatal(err)
		}
		if string(b) != tt.want {
			t.Errorf("%s = %s, want %s", tt.name, b, tt.want)
		}
	}
}

func TestInventoryHostPayload(t *testing.T) {
	g := gohai{
		CPU:      gohaiCPU{ModelName: "AMD64", VendorID: "GenuineIntel", CPUCores: 2, CPULogicalProcessors: 2, Mhz: 2400},
		Memory:   gohaiMemory{Total: 2 * 1024 * 1024 * 1024, SwapTotal: 1024}, // 2 GiB, 1 MB swap
		Platform: gohaiPlatform{OS: "plan9", KernelName: "Plan 9", Machine: "386"},
		Network:  gohaiNetwork{IPAddress: "10.0.2.15", MacAddress: "52:54:00:12:34:56"},
	}
	var s systemStats
	s.NixV = osVersion{"plan9", "9legacy", ""}

	body, err := json.Marshal(inventoryHost("test-host", g, s, 123))
	if err != nil {
		t.Fatal(err)
	}
	var p struct {
		Hostname  string `json:"hostname"`
		UUID      string `json:"uuid"`
		Timestamp int64  `json:"timestamp"`
		Metadata  struct {
			OS            string `json:"os"`
			AgentVersion  string `json:"agent_version"`
			MemoryTotalKb uint64 `json:"memory_total_kb"`
			CPUModel      string `json:"cpu_model"`
			OSVersion     string `json:"os_version"`
			MacAddress    string `json:"mac_address"`
		} `json:"host_metadata"`
	}
	if err := json.Unmarshal(body, &p); err != nil {
		t.Fatalf("inventory_host is not valid JSON: %v", err)
	}
	if p.Hostname != "test-host" || p.UUID == "" || p.Timestamp != 123 {
		t.Errorf("envelope = %+v", p)
	}
	if p.Metadata.OS != "plan9" {
		t.Errorf("os = %q, want plan9", p.Metadata.OS)
	}
	if p.Metadata.AgentVersion != intake.Version {
		t.Errorf("agent_version = %q, want %q", p.Metadata.AgentVersion, intake.Version)
	}
	if p.Metadata.MemoryTotalKb != 2*1024*1024 { // 2 GiB in kB
		t.Errorf("memory_total_kb = %d, want %d", p.Metadata.MemoryTotalKb, 2*1024*1024)
	}
	if p.Metadata.OSVersion != "9legacy" {
		t.Errorf("os_version = %q, want 9legacy", p.Metadata.OSVersion)
	}
	if p.Metadata.CPUModel != "AMD64" || p.Metadata.MacAddress == "" {
		t.Errorf("cpu_model/mac = %q/%q", p.Metadata.CPUModel, p.Metadata.MacAddress)
	}
}

func TestInventoryAgentPayload(t *testing.T) {
	body, err := json.Marshal(inventoryAgent("test-host", 123))
	if err != nil {
		t.Fatal(err)
	}
	var p struct {
		Hostname  string `json:"hostname"`
		UUID      string `json:"uuid"`
		Timestamp int64  `json:"timestamp"`
		Metadata  struct {
			AgentVersion       string `json:"agent_version"`
			Flavor             string `json:"flavor"`
			InfrastructureMode string `json:"infrastructure_mode"`
		} `json:"agent_metadata"`
	}
	if err := json.Unmarshal(body, &p); err != nil {
		t.Fatalf("inventory_agent is not valid JSON: %v", err)
	}
	if p.Hostname != "test-host" || p.UUID == "" || p.Timestamp != 123 {
		t.Errorf("envelope = %+v", p)
	}
	if p.Metadata.AgentVersion != intake.Version {
		t.Errorf("agent_version = %q, want %q", p.Metadata.AgentVersion, intake.Version)
	}
	if p.Metadata.Flavor != "dd-micro-agent" {
		t.Errorf("flavor = %q, want dd-micro-agent", p.Metadata.Flavor)
	}
	// The stock enum is full/end_user_device/basic/none, anything else is
	// coerced to full, so full is the value to send.
	if p.Metadata.InfrastructureMode != "full" {
		t.Errorf("infrastructure_mode = %q, want full", p.Metadata.InfrastructureMode)
	}
}

func TestSendPostsToIntake(t *testing.T) {
	var (
		path string
		body []byte
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		gr, _ := gzip.NewReader(r.Body)
		body, _ = io.ReadAll(gr)
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()

	r := New(Options{
		Client:          intake.New(intake.Options{}),
		IntakeEndpoints: []intake.Endpoint{{URL: srv.URL + "/intake/", APIKey: "k", Reliable: true}},
		Hostname:        "test-host",
		Tags:            []string{"env:test"},
	})
	r.send(context.Background())

	if path != "/intake/" {
		t.Errorf("path = %q, want /intake/", path)
	}
	var p struct {
		InternalHostname string `json:"internalHostname"`
	}
	if err := json.Unmarshal(body, &p); err != nil {
		t.Fatalf("server received non-JSON: %v", err)
	}
	if p.InternalHostname != "test-host" {
		t.Errorf("intake received internalHostname = %q", p.InternalHostname)
	}
}
