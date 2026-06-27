package logs

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/0intro/dd-micro-agent/internal/config"
)

// startTailer runs a tailer over path with a fast poll and returns its output
// channel. The tailer stops when the test ends.
func startTailer(t *testing.T, path string, reg *Registry) <-chan Message {
	t.Helper()
	out := make(chan Message, 64)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	tl := &tailer{
		source:   config.LogSource{Service: "svc", Source: "src"},
		id:       "file:" + path,
		path:     path,
		out:      out,
		registry: reg,
		poll:     10 * time.Millisecond,
		log:      discardLogger(),
	}
	go tl.run(ctx)
	return out
}

// A file with no registry entry is tailed from the end: pre-existing content is
// skipped (no history backfill), and only lines appended after the tailer opens
// it are shipped.
func TestTailerStartsAtEndThenReadsAppends(t *testing.T) {
	path := tmpFile(t, "old1\nold2\n") // pre-existing history, must NOT be shipped
	out := startTailer(t, path, emptyRegistry(t))
	expectNone(t, out, 200*time.Millisecond) // tail-from-end skips the history

	appendFile(t, path, "line3\n")
	if m := recv(t, out); string(m.Content) != "line3" || m.offset != 16 {
		t.Errorf("got %q off %d, want line3 off 16", m.Content, m.offset)
	}
}

// With a multi_line rule, a start line plus its continuation lines ship as one message.
func TestTailerMultiline(t *testing.T) {
	path := tmpFile(t, "")
	out := make(chan Message, 64)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	tl := &tailer{
		source: config.LogSource{
			Service: "svc", Source: "src",
			LogProcessingRules: []config.ProcessingRule{{Type: "multi_line", Pattern: `^\d{4}-`}},
		},
		id: "file:" + path, path: path, out: out,
		registry: emptyRegistry(t), poll: 10 * time.Millisecond, log: discardLogger(),
	}
	go tl.run(ctx)
	expectNone(t, out, 100*time.Millisecond) // tail from end

	appendFile(t, path, "2026-01-01 panic\n  at foo\n  at bar\n")
	want := "2026-01-01 panic\n  at foo\n  at bar"
	if m := recv(t, out); string(m.Content) != want {
		t.Errorf("aggregated message = %q, want %q", m.Content, want)
	}

	// A blank line inside the aggregate is part of the message, as in the stock Agent.
	appendFile(t, path, "2026-01-02 trace\n\n  at baz\n")
	want = "2026-01-02 trace\n\n  at baz"
	if m := recv(t, out); string(m.Content) != want {
		t.Errorf("aggregate with blank line = %q, want %q", m.Content, want)
	}
}

func TestTailerBuffersPartialLine(t *testing.T) {
	path := tmpFile(t, "") // empty, tailer starts at offset 0
	out := startTailer(t, path, emptyRegistry(t))
	expectNone(t, out, 100*time.Millisecond)

	appendFile(t, path, "partial") // no newline yet
	expectNone(t, out, 200*time.Millisecond)

	appendFile(t, path, "rest\n")
	if m := recv(t, out); string(m.Content) != "partialrest" || m.offset != 12 {
		t.Errorf("got %q off %d, want partialrest off 12", m.Content, m.offset)
	}
}

func TestTailerResumesFromRegistry(t *testing.T) {
	path := tmpFile(t, "line1\nline2\n")
	reg := emptyRegistry(t)
	reg.Advance("file:"+path, 6) // already read past line1

	out := startTailer(t, path, reg)
	if m := recv(t, out); string(m.Content) != "line2" {
		t.Errorf("got %q, want line2 (line1 already consumed)", m.Content)
	}
	expectNone(t, out, 100*time.Millisecond)
}

func TestTailerHandlesTruncation(t *testing.T) {
	path := tmpFile(t, "")
	out := startTailer(t, path, emptyRegistry(t))
	expectNone(t, out, 100*time.Millisecond)
	appendFile(t, path, "aaaa\nbbbb\n")
	recv(t, out) // aaaa
	recv(t, out) // bbbb

	// Truncate in place (same inode) and write a shorter line.
	if err := os.WriteFile(path, []byte("c\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if m := recv(t, out); string(m.Content) != "c" || m.offset != 2 {
		t.Errorf("got %q off %d, want c off 2 after truncation", m.Content, m.offset)
	}
}

func TestTailerHandlesRotation(t *testing.T) {
	path := tmpFile(t, "")
	out := startTailer(t, path, emptyRegistry(t))
	expectNone(t, out, 100*time.Millisecond)
	appendFile(t, path, "old1\nold2\n")
	recv(t, out) // old1
	recv(t, out) // old2

	// Rotate: move the file aside and create a fresh one at the same path.
	if err := os.Rename(path, path+".1"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("new1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if m := recv(t, out); string(m.Content) != "new1" || m.offset != 5 {
		t.Errorf("got %q off %d, want new1 off 5 after rotation", m.Content, m.offset)
	}
}

// A rotated file's unterminated last line ships before the tailer moves to the
// new file, since its bytes are about to be unreachable.
func TestTailerRotationShipsPartialLine(t *testing.T) {
	path := tmpFile(t, "")
	out := startTailer(t, path, emptyRegistry(t))
	expectNone(t, out, 100*time.Millisecond)
	appendFile(t, path, "done\nlast words") // no trailing newline
	if m := recv(t, out); string(m.Content) != "done" {
		t.Fatalf("got %q, want done", m.Content)
	}

	if err := os.Rename(path, path+".1"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("new\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if m := recv(t, out); string(m.Content) != "last words" || m.offset != 15 {
		t.Errorf("got %q off %d, want the unterminated tail 'last words' off 15", m.Content, m.offset)
	}
	if m := recv(t, out); string(m.Content) != "new" {
		t.Errorf("got %q, want new", m.Content)
	}
}

// A line that never sees its newline ships once it passes the cap, so a file of
// newline-free junk cannot grow the buffer without bound.
func TestTailerCapsRunawayLine(t *testing.T) {
	path := tmpFile(t, "")
	out := startTailer(t, path, emptyRegistry(t))
	expectNone(t, out, 100*time.Millisecond)

	big := strings.Repeat("x", maxLineBytes+1000)
	appendFile(t, path, big)
	total := 0
	var last Message
	for total < len(big) {
		last = recv(t, out)
		total += len(last.Content)
	}
	if total != len(big) || last.offset != int64(len(big)) {
		t.Errorf("shipped %d bytes ending at off %d, want %d at %d", total, last.offset, len(big), len(big))
	}
}

func TestTailerWaitsForMissingFile(t *testing.T) {
	path := tmpFile(t, "")
	if err := os.Remove(path); err != nil { // simulate "not yet there"
		t.Fatal(err)
	}
	out := startTailer(t, path, emptyRegistry(t))
	expectNone(t, out, 100*time.Millisecond)

	// Re-create empty so the tailer opens at its (zero) end, then append the line we
	// expect. Tail-from-end means content must arrive after the tailer is watching.
	if err := os.WriteFile(path, []byte{}, 0o644); err != nil {
		t.Fatal(err)
	}
	expectNone(t, out, 100*time.Millisecond)
	appendFile(t, path, "hello\n")
	if m := recv(t, out); string(m.Content) != "hello" {
		t.Errorf("got %q, want hello once the file appears", m.Content)
	}
}
