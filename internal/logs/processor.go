package logs

import (
	"bytes"
	"log/slog"
	"regexp"
	"time"

	"github.com/0intro/dd-micro-agent/internal/config"
)

// processor applies a source's log_processing_rules to tailed lines in two stages:
// optional multi_line aggregation (combine continuation lines into one message, keyed on
// a start-of-line pattern) followed by the mask_sequences / exclude_at_match /
// include_at_match rules in their configured order. It is stateful (the multiline
// buffer) and owned by a single tailer goroutine, so it needs no locking. With no
// rules configured every line passes straight through.
type processor struct {
	multiline    *regexp.Regexp // start-of-message pattern, nil disables aggregation
	flushTimeout time.Duration  // ship a buffered multiline message after this much idle
	rules        []rule         // mask/exclude/include, applied in config order

	pending     [][]byte  // buffered lines of the current multiline message
	pendingSize int       // bytes buffered, so the aggregate stays under maxLineBytes
	pendingOff  int64     // end offset of the last buffered line
	pendingTs   time.Time // when the last line was buffered (the flush timeout measures idle)
}

// rule is one mask_sequences, exclude_at_match, or include_at_match entry. The
// slice keeps the configured order, which is the order the stock Agent applies
// the rules in.
type rule struct {
	kind        string // the config type name
	re          *regexp.Regexp
	placeholder []byte // mask_sequences only
}

// newProcessor compiles the rules. A rule with an empty or invalid pattern is logged and
// skipped (the others still apply). With several multi_line rules the last one wins.
func newProcessor(rules []config.ProcessingRule, flushTimeout time.Duration, log *slog.Logger) *processor {
	if flushTimeout <= 0 {
		flushTimeout = defaultPollDelay
	}
	p := &processor{flushTimeout: flushTimeout}
	for _, r := range rules {
		if r.Pattern == "" {
			log.Warn("skipping log_processing_rule with empty pattern", "name", r.Name, "type", r.Type)
			continue
		}
		re, err := regexp.Compile(r.Pattern)
		if err != nil {
			log.Warn("skipping log_processing_rule with bad pattern", "name", r.Name, "type", r.Type, "err", err)
			continue
		}
		switch r.Type {
		case "multi_line":
			p.multiline = re
		case "mask_sequences":
			p.rules = append(p.rules, rule{kind: r.Type, re: re, placeholder: []byte(r.ReplacePlaceholder)})
		case "exclude_at_match", "include_at_match":
			p.rules = append(p.rules, rule{kind: r.Type, re: re})
		default:
			log.Warn("ignoring unknown log_processing_rule type", "name", r.Name, "type", r.Type)
		}
	}
	return p
}

// line feeds one raw line. Without multiline it returns the processed line immediately.
// With multiline, a line matching the start pattern flushes the buffered message
// (returned) and begins a new one. Other lines append. ok=true when a message is ready.
func (p *processor) line(raw []byte, offset int64, now time.Time) (out []byte, off int64, ok bool) {
	if p.multiline == nil {
		return p.finish(raw, offset)
	}
	if p.startsMessage(raw) && len(p.pending) > 0 {
		flushed, flushedOff := p.joined(), p.pendingOff
		p.reset()
		p.appendLine(raw, offset, now)
		return p.finish(flushed, flushedOff)
	}
	p.appendLine(raw, offset, now)
	if p.pendingSize > maxLineBytes {
		// The aggregate outgrew the line cap: ship it now so a runaway message
		// cannot grow the buffer without bound.
		flushed, flushedOff := p.joined(), p.pendingOff
		p.reset()
		return p.finish(flushed, flushedOff)
	}
	return nil, 0, false
}

// flush returns the buffered multiline message once it has been idle for the flush
// timeout, or immediately when force (file rotation). ok=false if nothing is pending.
func (p *processor) flush(now time.Time, force bool) (out []byte, off int64, ok bool) {
	if len(p.pending) == 0 {
		return nil, 0, false
	}
	if !force && now.Sub(p.pendingTs) < p.flushTimeout {
		return nil, 0, false
	}
	flushed, flushedOff := p.joined(), p.pendingOff
	p.reset()
	return p.finish(flushed, flushedOff)
}

// startsMessage reports whether raw begins a new logical message: the multiline pattern
// matches at the very start of the line, so a timestamp/marker appearing mid-continuation
// doesn't falsely split a message. This mirrors the stock Agent, which anchors multi_line
// at the line start whether or not the pattern itself includes ^.
func (p *processor) startsMessage(raw []byte) bool {
	loc := p.multiline.FindIndex(raw)
	return loc != nil && loc[0] == 0
}

func (p *processor) appendLine(raw []byte, offset int64, now time.Time) {
	p.pending = append(p.pending, append([]byte(nil), raw...))
	p.pendingSize += len(raw) + 1
	p.pendingOff = offset
	p.pendingTs = now // per line, so the flush timeout measures idle time
}

func (p *processor) joined() []byte { return bytes.Join(p.pending, []byte{'\n'}) }

func (p *processor) reset() { p.pending, p.pendingSize, p.pendingOff = nil, 0, 0 }

// aggregating reports whether a multi_line rule is active, in which case empty
// lines are part of the aggregate and must reach line.
func (p *processor) aggregating() bool { return p.multiline != nil }

// finish applies the mask/exclude/include rules to a completed logical line, in
// the order they were configured, like the stock Agent. ok=false when an
// exclude_at_match hits or an include_at_match misses.
func (p *processor) finish(content []byte, offset int64) (out []byte, off int64, ok bool) {
	for _, r := range p.rules {
		switch r.kind {
		case "mask_sequences":
			content = r.re.ReplaceAll(content, r.placeholder)
		case "exclude_at_match":
			if r.re.Match(content) {
				return nil, 0, false
			}
		case "include_at_match":
			if !r.re.Match(content) {
				return nil, 0, false
			}
		}
	}
	return content, offset, true
}
