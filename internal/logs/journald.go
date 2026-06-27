package logs

import (
	"encoding/json"
	"strconv"
	"strings"

	"github.com/0intro/dd-micro-agent/internal/config"
)

// journald support reads the systemd journal by exec'ing journalctl (the tailer
// itself lives in journald_linux.go). This file is the pure core: turning one
// journalctl "-o json" line into the Message body the stock Agent ships byte for
// byte, the PRIORITY-to-status map, the service-name rule, the filter checks, and
// the journalctl argument list. It is build-tag-neutral so it unit-tests on any
// host, the same split as internal/process proto.go and host plan9parse.go.

// priorityStatus maps a syslog severity (journald PRIORITY, "0" to "7") to a
// Datadog status. A missing or unknown value is "info", as in the stock Agent.
func priorityStatus(priority string) string {
	switch priority {
	case "0":
		return "emergency"
	case "1":
		return "alert"
	case "2":
		return "critical"
	case "3":
		return "error"
	case "4":
		return "warn"
	case "5":
		return "notice"
	case "6":
		return "info"
	case "7":
		return "debug"
	}
	return "info"
}

// applicationKeys is the stock Agent's order for naming the source of an entry.
var applicationKeys = []string{"SYSLOG_IDENTIFIER", "_SYSTEMD_USER_UNIT", "_SYSTEMD_UNIT", "_COMM"}

// deriveService returns the application name for an entry: the first present of
// applicationKeys, matching the stock Agent. The caller overrides it with the
// source's service/source when those are configured.
func deriveService(fields map[string]string) string {
	for _, k := range applicationKeys {
		if v := fields[k]; v != "" {
			return v
		}
	}
	return ""
}

// parseEntry decodes one journalctl "-o json" line. It returns the entry's data
// fields with the journal address fields (those starting with "__") removed, plus
// the cursor and realtime timestamp lifted from them. Removing the "__" fields is
// what makes the field set match the stock Agent's sdjournal entry.Fields, which
// exposes the cursor and timestamps through separate calls, not as data.
func parseEntry(line []byte) (fields map[string]string, cursor string, realtime int64, err error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(line, &raw); err != nil {
		return nil, "", 0, err
	}
	fields = make(map[string]string, len(raw))
	for k, v := range raw {
		s := fieldString(v)
		switch k {
		case "__CURSOR":
			cursor = s
		case "__REALTIME_TIMESTAMP":
			realtime, _ = strconv.ParseInt(s, 10, 64)
		}
		if strings.HasPrefix(k, "__") {
			continue
		}
		fields[k] = s
	}
	return fields, cursor, realtime, nil
}

// fieldString renders a journalctl JSON field value as a string. A plain JSON
// string is used directly. A JSON array is journalctl's encoding of a binary
// value (an array of byte values), which we decode to the raw bytes as a string,
// matching how the stock Agent's map[string]string holds the same field.
func fieldString(v json.RawMessage) string {
	if len(v) == 0 {
		return ""
	}
	switch v[0] {
	case '"':
		var s string
		if json.Unmarshal(v, &s) == nil {
			return s
		}
	case '[':
		var nums []byte
		if json.Unmarshal(v, &nums) == nil {
			return string(nums)
		}
	}
	return string(v)
}

// buildBody renders an entry as the structured JSON the stock Agent ships by
// default (process_raw_message true): {"journald":{<fields minus MESSAGE>},
// "message":"<MESSAGE>"}. Go sorts map keys when marshaling, so the bytes are
// deterministic and match the stock Agent's json.Marshal of the same map. The
// "message" key is present only when the entry carries a MESSAGE, as in the stock
// Agent.
func buildBody(fields map[string]string) []byte {
	journald := make(map[string]string, len(fields))
	for k, v := range fields {
		if k == "MESSAGE" {
			continue
		}
		journald[k] = v
	}
	data := map[string]any{"journald": journald}
	if msg, ok := fields["MESSAGE"]; ok {
		data["message"] = msg
	}
	body, _ := json.Marshal(data)
	return body
}

// shouldDrop reports whether an entry is filtered out by the source's
// exclude_units / exclude_user_units / exclude_matches, mirroring the stock
// Agent's runtime drop. An entry carrying _SYSTEMD_USER_UNIT is a user-level unit
// and only the user excludes apply, otherwise the system excludes apply. A "*"
// exclude drops every entry at that level.
func shouldDrop(src config.LogSource, fields map[string]string) bool {
	for _, m := range src.ExcludeMatches {
		if k, v, ok := splitMatch(m); ok && fields[k] == v {
			return true
		}
	}
	if usr, ok := fields["_SYSTEMD_USER_UNIT"]; ok {
		return matchesUnit(src.ExcludeUserUnits, usr)
	}
	if sys, ok := fields["_SYSTEMD_UNIT"]; ok {
		return matchesUnit(src.ExcludeUnits, sys)
	}
	return false
}

func matchesUnit(excludes []string, unit string) bool {
	for _, e := range excludes {
		if e == "*" || e == unit {
			return true
		}
	}
	return false
}

func splitMatch(m string) (key, value string, ok bool) {
	i := strings.IndexByte(m, '=')
	if i <= 0 {
		return "", "", false
	}
	return m[:i], m[i+1:], true
}

// journalctlArgs builds the journalctl command line for a source. --all keeps
// large fields inline: without it journalctl replaces any field over 4 KiB with
// JSON null, while the stock Agent's sdjournal reads the full value. cursor is
// the last delivered entry's cursor, empty on first start. With a cursor we
// resume right after it. Without one we honor start_position: "beginning" reads
// the whole journal, anything else (the default "end") follows only new entries.
// include_units, include_user_units, and include_matches become journalctl match
// arguments. journalctl ORs matches on the same field and ANDs across fields, so
// the two unit groups are joined by a "+" disjunction. Excludes have no native
// journalctl form and are applied after decode (shouldDrop), as the stock Agent
// does.
func journalctlArgs(src config.LogSource, cursor string) []string {
	args := []string{"--output=json", "--all", "--follow", "--quiet", "--no-pager"}
	if src.Path != "" {
		args = append(args, "--directory="+src.Path)
	}
	switch {
	case cursor != "":
		args = append(args, "--after-cursor="+cursor)
	case src.StartPosition == "beginning" || src.StartPosition == "forceBeginning":
		args = append(args, "--no-tail")
	default:
		args = append(args, "--lines=0") // end: follow only entries after start
	}
	for _, m := range src.IncludeMatches {
		args = append(args, m)
	}
	sys := unitMatches("_SYSTEMD_UNIT", src.IncludeUnits)
	usr := unitMatches("_SYSTEMD_USER_UNIT", src.IncludeUserUnits)
	switch {
	case len(sys) > 0 && len(usr) > 0:
		args = append(args, sys...)
		args = append(args, "+")
		args = append(args, usr...)
	default:
		args = append(args, sys...)
		args = append(args, usr...)
	}
	return args
}

func unitMatches(field string, units []string) []string {
	var out []string
	for _, u := range units {
		out = append(out, field+"="+u)
	}
	return out
}
