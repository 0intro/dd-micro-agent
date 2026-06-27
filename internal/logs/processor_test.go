package logs

import (
	"bytes"
	"testing"
	"time"

	"github.com/0intro/dd-micro-agent/internal/config"
)

func TestProcessorMultiline(t *testing.T) {
	p := newProcessor([]config.ProcessingRule{
		{Type: "multi_line", Name: "ts", Pattern: `^\d{4}-\d{2}-\d{2}`},
	}, 10*time.Millisecond, discardLogger())
	now := time.Unix(0, 0)

	if _, _, ok := p.line([]byte("2026-01-01 start"), 10, now); ok {
		t.Fatal("first start line should buffer, not emit")
	}
	if _, _, ok := p.line([]byte("  at foo()"), 20, now); ok {
		t.Fatal("continuation should buffer")
	}
	if _, _, ok := p.line([]byte("  at bar()"), 30, now); ok {
		t.Fatal("continuation should buffer")
	}
	// A new start line flushes the previous aggregate (offset = last buffered line).
	out, off, ok := p.line([]byte("2026-01-01 next"), 40, now)
	want := "2026-01-01 start\n  at foo()\n  at bar()"
	if !ok || string(out) != want || off != 30 {
		t.Errorf("aggregated = %q off %d ok %v, want %q off 30", out, off, ok, want)
	}
	// The last buffered message ships after the idle timeout.
	if out, off, ok := p.flush(now.Add(time.Second), false); !ok || string(out) != "2026-01-01 next" || off != 40 {
		t.Errorf("flush = %q off %d ok %v, want '2026-01-01 next' off 40", out, off, ok)
	}
	if _, _, ok := p.flush(now.Add(2*time.Second), true); ok {
		t.Error("flush after drain should be empty")
	}
}

func TestProcessorMultilineAnchored(t *testing.T) {
	// An un-anchored pattern still only starts a message at the line start, so a date
	// appearing mid-continuation must not split the message.
	p := newProcessor([]config.ProcessingRule{{Type: "multi_line", Pattern: `\d{4}-\d{2}-\d{2}`}}, time.Second, discardLogger())
	now := time.Unix(0, 0)
	p.line([]byte("2026-01-01 start"), 10, now)
	if _, _, ok := p.line([]byte("  saw 2026-02-02 inline"), 20, now); ok {
		t.Error("a date mid-continuation must not start a new message")
	}
	if out, _, ok := p.flush(now.Add(2*time.Second), true); !ok || string(out) != "2026-01-01 start\n  saw 2026-02-02 inline" {
		t.Errorf("aggregate = %q ok %v", out, ok)
	}
}

func TestProcessorFlushTimeout(t *testing.T) {
	p := newProcessor([]config.ProcessingRule{{Type: "multi_line", Pattern: "^X"}}, time.Second, discardLogger())
	start := time.Unix(100, 0)
	p.line([]byte("X first"), 5, start)
	if _, _, ok := p.flush(start.Add(500*time.Millisecond), false); ok {
		t.Error("should not flush before the idle timeout")
	}
	if _, _, ok := p.flush(start.Add(2*time.Second), false); !ok {
		t.Error("should flush after the idle timeout")
	}
}

// The flush timeout measures idle time since the last buffered line, not the age
// of the message, so a slowly arriving continuation keeps the aggregate open, as
// in the stock Agent.
func TestProcessorFlushTimeoutMeasuresIdle(t *testing.T) {
	p := newProcessor([]config.ProcessingRule{{Type: "multi_line", Pattern: "^X"}}, time.Second, discardLogger())
	start := time.Unix(100, 0)
	p.line([]byte("X first"), 5, start)
	p.line([]byte("  more"), 12, start.Add(900*time.Millisecond))
	if _, _, ok := p.flush(start.Add(1500*time.Millisecond), false); ok {
		t.Error("a recent continuation must defer the idle flush")
	}
	if out, _, ok := p.flush(start.Add(2*time.Second), false); !ok || string(out) != "X first\n  more" {
		t.Errorf("flush = %q ok %v, want the aggregate once idle", out, ok)
	}
}

// An aggregate that outgrows the line cap ships immediately so the buffer stays
// bounded on a runaway message.
func TestProcessorMultilineCapped(t *testing.T) {
	p := newProcessor([]config.ProcessingRule{{Type: "multi_line", Pattern: "^X"}}, time.Second, discardLogger())
	now := time.Unix(0, 0)
	p.line([]byte("X start"), 10, now)
	big := bytes.Repeat([]byte("a"), maxLineBytes)
	out, off, ok := p.line(big, 20, now)
	if !ok || off != 20 {
		t.Fatalf("oversized aggregate: ok %v off %d, want an immediate flush at off 20", ok, off)
	}
	if len(out) <= maxLineBytes {
		t.Errorf("flushed %d bytes, want the whole aggregate", len(out))
	}
	if len(p.pending) != 0 {
		t.Error("buffer must be empty after the capped flush")
	}
}

func TestProcessorRules(t *testing.T) {
	now := time.Unix(0, 0)

	mask := newProcessor([]config.ProcessingRule{
		{Type: "mask_sequences", Pattern: `password=\S+`, ReplacePlaceholder: "password=[REDACTED]"},
	}, time.Second, discardLogger())
	if out, _, ok := mask.line([]byte("login password=hunter2 ok"), 1, now); !ok || string(out) != "login password=[REDACTED] ok" {
		t.Errorf("mask = %q ok %v", out, ok)
	}

	excl := newProcessor([]config.ProcessingRule{{Type: "exclude_at_match", Pattern: `DEBUG`}}, time.Second, discardLogger())
	if _, _, ok := excl.line([]byte("DEBUG noisy"), 1, now); ok {
		t.Error("exclude should drop the matching line")
	}
	if _, _, ok := excl.line([]byte("ERROR real"), 1, now); !ok {
		t.Error("exclude should keep non-matching lines")
	}

	incl := newProcessor([]config.ProcessingRule{{Type: "include_at_match", Pattern: `ERROR`}}, time.Second, discardLogger())
	if _, _, ok := incl.line([]byte("ERROR keep"), 1, now); !ok {
		t.Error("include should keep matching lines")
	}
	if _, _, ok := incl.line([]byte("INFO drop"), 1, now); ok {
		t.Error("include should drop non-matching lines")
	}
}

// Rules apply in their configured order, like the stock Agent: a mask listed
// before an exclude runs first, so the exclude sees the masked content.
func TestProcessorRuleOrder(t *testing.T) {
	now := time.Unix(0, 0)
	maskFirst := newProcessor([]config.ProcessingRule{
		{Type: "mask_sequences", Pattern: `secret`, ReplacePlaceholder: "[HIDDEN]"},
		{Type: "exclude_at_match", Pattern: `secret`},
	}, time.Second, discardLogger())
	if out, _, ok := maskFirst.line([]byte("a secret thing"), 1, now); !ok || string(out) != "a [HIDDEN] thing" {
		t.Errorf("line = %q ok %v, want the masked line kept (mask ran before exclude)", out, ok)
	}

	exclFirst := newProcessor([]config.ProcessingRule{
		{Type: "exclude_at_match", Pattern: `secret`},
		{Type: "mask_sequences", Pattern: `secret`, ReplacePlaceholder: "[HIDDEN]"},
	}, time.Second, discardLogger())
	if _, _, ok := exclFirst.line([]byte("a secret thing"), 1, now); ok {
		t.Error("an exclude listed first must drop the raw line")
	}
}

func TestProcessorBadPatternSkipped(t *testing.T) {
	p := newProcessor([]config.ProcessingRule{
		{Type: "exclude_at_match", Pattern: "(unclosed"},
		{Type: "mask_sequences", Pattern: "x", ReplacePlaceholder: "Y"},
	}, time.Second, discardLogger())
	if len(p.rules) != 1 {
		t.Errorf("rules = %d, want 1 (bad exclude regex skipped)", len(p.rules))
	}
	if out, _, ok := p.line([]byte("xx"), 1, time.Unix(0, 0)); !ok || string(out) != "YY" {
		t.Errorf("good mask rule should still apply: %q", out)
	}
}
