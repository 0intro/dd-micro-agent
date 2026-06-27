// Package dogstatsd implements the DogStatsD intake: UDP and Unix-domain-socket
// listeners that parse metric lines and hand the samples to an aggregator.
package dogstatsd

import (
	"strconv"
	"strings"

	"github.com/0intro/dd-micro-agent/internal/metrics"
)

// parse converts one DogStatsD line into samples. The wire format is
//
//	name:value[:value...]|type[|@sample_rate][|#tag1:v1,tag2:v2][|...]
//
// A packed line (name:1:2:3|h) yields one sample per value, as in the stock
// Agent. A set's value is a single arbitrary string, so it is never split.
// Unknown trailing fields (container id, timestamp) are ignored. ok is false for
// a malformed line, which the caller logs and drops.
func parse(line string) ([]metrics.Sample, bool) {
	head, rest, ok := strings.Cut(line, "|")
	if !ok {
		return nil, false
	}
	name, valueStr, ok := strings.Cut(head, ":")
	if !ok || name == "" {
		return nil, false
	}

	typeStr, opts, _ := strings.Cut(rest, "|")
	typ, ok := parseType(typeStr)
	if !ok {
		return nil, false
	}

	base := metrics.Sample{Name: name, Type: typ, SampleRate: 1}
	for opts != "" {
		var field string
		field, opts, _ = strings.Cut(opts, "|")
		switch {
		case strings.HasPrefix(field, "@"):
			if r, err := strconv.ParseFloat(field[1:], 64); err == nil && r > 0 {
				base.SampleRate = r
			}
		case strings.HasPrefix(field, "#"):
			base.Tags = splitTags(field[1:])
		}
	}

	if typ == metrics.SampleSet {
		base.Str = valueStr // counted distinct, may itself contain ':'
		return []metrics.Sample{base}, true
	}
	var samples []metrics.Sample
	for {
		vs, more, packed := strings.Cut(valueStr, ":")
		v, err := strconv.ParseFloat(vs, 64)
		if err != nil {
			return nil, false
		}
		s := base
		s.Value = v
		samples = append(samples, s)
		if !packed {
			return samples, true
		}
		valueStr = more
	}
}

func parseType(s string) (metrics.SampleType, bool) {
	switch s {
	case "g":
		return metrics.SampleGauge, true
	case "c":
		return metrics.SampleCounter, true
	case "h":
		return metrics.SampleHistogram, true
	case "ms":
		return metrics.SampleTiming, true
	case "d":
		return metrics.SampleDistribution, true
	case "s":
		return metrics.SampleSet, true
	}
	return 0, false
}

func splitTags(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(s, ",")
}

// parseServiceCheck parses a DogStatsD service-check datagram:
//
//	_sc|<name>|<status>[|d:<unix>][|h:<host>][|#<tags>][|m:<message>]
//
// status is 0=OK 1=WARNING 2=CRITICAL 3=UNKNOWN. The message (m:) is last and
// may contain pipes. ok is false for a malformed line.
func parseServiceCheck(line string) (metrics.ServiceCheck, bool) {
	rest, ok := strings.CutPrefix(line, "_sc|")
	if !ok {
		return metrics.ServiceCheck{}, false
	}
	name, rest, ok := strings.Cut(rest, "|")
	if !ok || name == "" {
		return metrics.ServiceCheck{}, false
	}
	statusStr, opts, _ := strings.Cut(rest, "|")
	status, err := strconv.Atoi(statusStr)
	if err != nil || status < 0 || status > 3 {
		return metrics.ServiceCheck{}, false
	}
	sc := metrics.ServiceCheck{Check: name, Status: metrics.ServiceCheckStatus(status)}
	for opts != "" {
		var field string
		field, opts, _ = strings.Cut(opts, "|")
		switch {
		case strings.HasPrefix(field, "d:"):
			if ts, err := strconv.ParseInt(field[2:], 10, 64); err == nil {
				sc.Timestamp = ts
			}
		case strings.HasPrefix(field, "h:"):
			sc.Host = field[2:]
		case strings.HasPrefix(field, "#"):
			sc.Tags = splitTags(field[1:])
		case strings.HasPrefix(field, "m:"):
			sc.Message = field[2:] // m: is last, so restore any pipes the Cut split off
			if opts != "" {
				sc.Message += "|" + opts
				opts = ""
			}
		}
	}
	return sc, true
}

// parseEvent parses a DogStatsD event datagram:
//
//	_e{<title.len>,<text.len>}:<title>|<text>[|d:<unix>][|h:<host>][|p:<priority>]
//	   [|t:<alert_type>][|s:<source>][|k:<agg_key>][|#<tags>]
//
// The {len,len} frame lets the title and text contain pipes, and both encode
// newlines as the two characters "\n". An empty title is malformed, as in the
// stock Agent. ok is false for a malformed line.
func parseEvent(line string) (metrics.Event, bool) {
	rest, ok := strings.CutPrefix(line, "_e{")
	if !ok {
		return metrics.Event{}, false
	}
	header, body, ok := strings.Cut(rest, "}:")
	if !ok {
		return metrics.Event{}, false
	}
	tlStr, elStr, ok := strings.Cut(header, ",")
	if !ok {
		return metrics.Event{}, false
	}
	tl, err1 := strconv.Atoi(tlStr)
	el, err2 := strconv.Atoi(elStr)
	if err1 != nil || err2 != nil || tl <= 0 || el < 0 {
		return metrics.Event{}, false
	}
	if len(body) < tl {
		return metrics.Event{}, false
	}
	title, r := body[:tl], body[tl:]
	r, ok = strings.CutPrefix(r, "|")
	if !ok || len(r) < el {
		return metrics.Event{}, false
	}
	text, opts := r[:el], r[el:]
	unescape := func(s string) string { return strings.ReplaceAll(s, `\n`, "\n") }
	ev := metrics.Event{Title: unescape(title), Text: unescape(text)}
	for opts != "" {
		opts = strings.TrimPrefix(opts, "|")
		var field string
		field, opts, _ = strings.Cut(opts, "|")
		switch {
		case strings.HasPrefix(field, "d:"):
			if ts, err := strconv.ParseInt(field[2:], 10, 64); err == nil {
				ev.Timestamp = ts
			}
		case strings.HasPrefix(field, "h:"):
			ev.Host = field[2:]
		case strings.HasPrefix(field, "p:"):
			ev.Priority = field[2:]
		case strings.HasPrefix(field, "t:"):
			ev.AlertType = field[2:]
		case strings.HasPrefix(field, "s:"):
			ev.SourceTypeName = field[2:]
		case strings.HasPrefix(field, "k:"):
			ev.AggregationKey = field[2:]
		case strings.HasPrefix(field, "#"):
			ev.Tags = splitTags(field[1:])
		}
	}
	return ev, true
}
