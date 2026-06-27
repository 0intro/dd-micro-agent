package logs

import (
	"reflect"
	"testing"

	"github.com/0intro/dd-micro-agent/internal/config"
)

// realLine is a captured journalctl "-o json" entry from a Fedora host. realBody
// is the structured message the stock Agent ships for it by default, computed
// independently (sorted keys, MESSAGE lifted, "__" address fields removed). The
// two together pin byte-parity with the stock Agent's wire format.
const realLine = `{"__SEQNUM":"26628118","_CAP_EFFECTIVE":"1ffffffffff","_COMM":"systemd","_TRANSPORT":"journal","MESSAGE":"setroubleshootd.service: Deactivated successfully.","CODE_FILE":"src/core/unit.c","TID":"1","__REALTIME_TIMESTAMP":"1782840698615019","CODE_FUNC":"unit_log_success","_CMDLINE":"/usr/lib/systemd/systemd --switched-root --system --deserialize=57 rhgb","__CURSOR":"s=8fd566c73958435f963863d2ae2baed5;i=1965016;b=47aedb40ec6646cebbad8bbe68636bb6;m=271817ad520;t=6557bf28834eb;x=bcbb495012dd89ec","_MACHINE_ID":"4542c9de1bb24230b4c64d06e27a79e2","_RUNTIME_SCOPE":"system","_EXE":"/usr/lib/systemd/systemd","MESSAGE_ID":"7ad2d189f7e94e70a38c781354912448","__SEQNUM_ID":"8fd566c73958435f963863d2ae2baed5","_PID":"1","_SYSTEMD_CGROUP":"/init.scope","UNIT":"setroubleshootd.service","_SYSTEMD_SLICE":"-.slice","_HOSTNAME":"iron.9fans.fr","_BOOT_ID":"47aedb40ec6646cebbad8bbe68636bb6","INVOCATION_ID":"141eac240efc42659d18fa19d2dc990b","_GID":"0","_UID":"0","SYSLOG_IDENTIFIER":"systemd","SYSLOG_FACILITY":"3","PRIORITY":"6","_SYSTEMD_UNIT":"init.scope","__MONOTONIC_TIMESTAMP":"2686526870816","_SELINUX_CONTEXT":"system_u:system_r:init_t:s0","CODE_LINE":"6168","_SOURCE_REALTIME_TIMESTAMP":"1782840698615011"}`

const realBody = `{"journald":{"CODE_FILE":"src/core/unit.c","CODE_FUNC":"unit_log_success","CODE_LINE":"6168","INVOCATION_ID":"141eac240efc42659d18fa19d2dc990b","MESSAGE_ID":"7ad2d189f7e94e70a38c781354912448","PRIORITY":"6","SYSLOG_FACILITY":"3","SYSLOG_IDENTIFIER":"systemd","TID":"1","UNIT":"setroubleshootd.service","_BOOT_ID":"47aedb40ec6646cebbad8bbe68636bb6","_CAP_EFFECTIVE":"1ffffffffff","_CMDLINE":"/usr/lib/systemd/systemd --switched-root --system --deserialize=57 rhgb","_COMM":"systemd","_EXE":"/usr/lib/systemd/systemd","_GID":"0","_HOSTNAME":"iron.9fans.fr","_MACHINE_ID":"4542c9de1bb24230b4c64d06e27a79e2","_PID":"1","_RUNTIME_SCOPE":"system","_SELINUX_CONTEXT":"system_u:system_r:init_t:s0","_SOURCE_REALTIME_TIMESTAMP":"1782840698615011","_SYSTEMD_CGROUP":"/init.scope","_SYSTEMD_SLICE":"-.slice","_SYSTEMD_UNIT":"init.scope","_TRANSPORT":"journal","_UID":"0"},"message":"setroubleshootd.service: Deactivated successfully."}`

func TestParseEntryRealLine(t *testing.T) {
	fields, cursor, realtime, err := parseEntry([]byte(realLine))
	if err != nil {
		t.Fatalf("parseEntry: %v", err)
	}
	for k := range fields {
		if len(k) >= 2 && k[:2] == "__" {
			t.Errorf("address field %q leaked into data fields", k)
		}
	}
	if want := "s=8fd566c73958435f963863d2ae2baed5;i=1965016;b=47aedb40ec6646cebbad8bbe68636bb6;m=271817ad520;t=6557bf28834eb;x=bcbb495012dd89ec"; cursor != want {
		t.Errorf("cursor = %q, want %q", cursor, want)
	}
	if realtime != 1782840698615019 {
		t.Errorf("realtime = %d, want 1782840698615019", realtime)
	}
	if fields["MESSAGE"] != "setroubleshootd.service: Deactivated successfully." {
		t.Errorf("MESSAGE = %q", fields["MESSAGE"])
	}
}

// TestBuildBodyParity is the byte-parity guard: the body we ship for a real entry
// must equal what the stock Agent ships by default.
func TestBuildBodyParity(t *testing.T) {
	fields, _, _, err := parseEntry([]byte(realLine))
	if err != nil {
		t.Fatalf("parseEntry: %v", err)
	}
	if got := string(buildBody(fields)); got != realBody {
		t.Errorf("buildBody mismatch\n got: %s\nwant: %s", got, realBody)
	}
}

func TestBuildBody(t *testing.T) {
	tests := []struct {
		name   string
		fields map[string]string
		want   string
	}{
		{
			"message and one field",
			map[string]string{"MESSAGE": "hello", "_SYSTEMD_UNIT": "ssh.service"},
			`{"journald":{"_SYSTEMD_UNIT":"ssh.service"},"message":"hello"}`,
		},
		{
			"keys are sorted",
			map[string]string{"MESSAGE": "x", "PRIORITY": "3", "_COMM": "sshd", "SYSLOG_IDENTIFIER": "sshd"},
			`{"journald":{"PRIORITY":"3","SYSLOG_IDENTIFIER":"sshd","_COMM":"sshd"},"message":"x"}`,
		},
		{
			"no MESSAGE means no message key",
			map[string]string{"_SYSTEMD_UNIT": "audit.service", "PRIORITY": "5"},
			`{"journald":{"PRIORITY":"5","_SYSTEMD_UNIT":"audit.service"}}`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := string(buildBody(tt.fields)); got != tt.want {
				t.Errorf("buildBody = %s, want %s", got, tt.want)
			}
		})
	}
}

func TestFieldStringBinary(t *testing.T) {
	// journalctl encodes a non-UTF8 field as an array of byte values.
	if got := fieldString([]byte(`[104,105]`)); got != "hi" {
		t.Errorf("binary field = %q, want %q", got, "hi")
	}
	if got := fieldString([]byte(`"plain"`)); got != "plain" {
		t.Errorf("string field = %q, want %q", got, "plain")
	}
}

func TestPriorityStatus(t *testing.T) {
	tests := map[string]string{
		"0": "emergency", "1": "alert", "2": "critical", "3": "error",
		"4": "warn", "5": "notice", "6": "info", "7": "debug",
		"": "info", "9": "info", "x": "info",
	}
	for in, want := range tests {
		if got := priorityStatus(in); got != want {
			t.Errorf("priorityStatus(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestDeriveService(t *testing.T) {
	tests := []struct {
		name   string
		fields map[string]string
		want   string
	}{
		{"syslog identifier wins", map[string]string{"SYSLOG_IDENTIFIER": "sshd", "_SYSTEMD_UNIT": "ssh.service", "_COMM": "sshd"}, "sshd"},
		{"user unit over system unit", map[string]string{"_SYSTEMD_USER_UNIT": "app.service", "_SYSTEMD_UNIT": "user@1000.service", "_COMM": "app"}, "app.service"},
		{"system unit over comm", map[string]string{"_SYSTEMD_UNIT": "cron.service", "_COMM": "cron"}, "cron.service"},
		{"comm last", map[string]string{"_COMM": "bash"}, "bash"},
		{"none", map[string]string{"PRIORITY": "6"}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := deriveService(tt.fields); got != tt.want {
				t.Errorf("deriveService = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestShouldDrop(t *testing.T) {
	tests := []struct {
		name   string
		src    config.LogSource
		fields map[string]string
		want   bool
	}{
		{"exclude match hits", config.LogSource{ExcludeMatches: []string{"PRIORITY=6"}}, map[string]string{"PRIORITY": "6"}, true},
		{"exclude match misses", config.LogSource{ExcludeMatches: []string{"PRIORITY=3"}}, map[string]string{"PRIORITY": "6"}, false},
		{"exclude system unit", config.LogSource{ExcludeUnits: []string{"noisy.service"}}, map[string]string{"_SYSTEMD_UNIT": "noisy.service"}, true},
		{"exclude all system units", config.LogSource{ExcludeUnits: []string{"*"}}, map[string]string{"_SYSTEMD_UNIT": "any.service"}, true},
		{"system exclude does not drop user entry", config.LogSource{ExcludeUnits: []string{"*"}}, map[string]string{"_SYSTEMD_UNIT": "user@1000.service", "_SYSTEMD_USER_UNIT": "app.service"}, false},
		{"exclude user unit", config.LogSource{ExcludeUserUnits: []string{"app.service"}}, map[string]string{"_SYSTEMD_UNIT": "user@1000.service", "_SYSTEMD_USER_UNIT": "app.service"}, true},
		{"nothing configured", config.LogSource{}, map[string]string{"_SYSTEMD_UNIT": "ssh.service"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := shouldDrop(tt.src, tt.fields); got != tt.want {
				t.Errorf("shouldDrop = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestJournalctlArgs(t *testing.T) {
	base := []string{"--output=json", "--all", "--follow", "--quiet", "--no-pager"}
	tests := []struct {
		name   string
		src    config.LogSource
		cursor string
		want   []string
	}{
		{"default is end", config.LogSource{}, "", append(clone(base), "--lines=0")},
		{"beginning reads all", config.LogSource{StartPosition: "beginning"}, "", append(clone(base), "--no-tail")},
		{"cursor resumes", config.LogSource{StartPosition: "beginning"}, "s=abc;i=1", append(clone(base), "--after-cursor=s=abc;i=1")},
		{"directory", config.LogSource{Path: "/var/log/journal/x"}, "", append(clone(base), "--directory=/var/log/journal/x", "--lines=0")},
		{"include matches", config.LogSource{IncludeMatches: []string{"SYSLOG_IDENTIFIER=sshd"}}, "", append(clone(base), "--lines=0", "SYSLOG_IDENTIFIER=sshd")},
		{"include units", config.LogSource{IncludeUnits: []string{"ssh.service", "cron.service"}}, "", append(clone(base), "--lines=0", "_SYSTEMD_UNIT=ssh.service", "_SYSTEMD_UNIT=cron.service")},
		{"system and user units are disjoined", config.LogSource{IncludeUnits: []string{"ssh.service"}, IncludeUserUnits: []string{"app.service"}}, "", append(clone(base), "--lines=0", "_SYSTEMD_UNIT=ssh.service", "+", "_SYSTEMD_USER_UNIT=app.service")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := journalctlArgs(tt.src, tt.cursor); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("journalctlArgs =\n %v\nwant\n %v", got, tt.want)
			}
		})
	}
}

func clone(s []string) []string { return append([]string(nil), s...) }
