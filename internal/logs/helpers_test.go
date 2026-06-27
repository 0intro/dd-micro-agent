package logs

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// logBuffer collects the agent's slog output so a test can wait for an emitted
// line instead of sleeping.
type logBuffer struct {
	mu sync.Mutex
	b  strings.Builder
}

func (l *logBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *logBuffer) contains(s string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.Contains(l.b.String(), s)
}

func (l *logBuffer) count(s string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.Count(l.b.String(), s)
}

// captureLogger returns a debug-level logger and the buffer it writes to.
func captureLogger() (*logBuffer, *slog.Logger) {
	buf := &logBuffer{}
	return buf, slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

// tailing reports whether buf has logged the tailer's post-seek line for path,
// after which appended lines are guaranteed to be past the tail-from-end seek.
func tailing(buf *logBuffer, path string) func() bool {
	return func() bool { return buf.contains(`msg="tailing from" path=` + path) }
}

func recv(t *testing.T, ch <-chan Message) Message {
	t.Helper()
	select {
	case m := <-ch:
		return m
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for a message")
		return Message{}
	}
}

func expectNone(t *testing.T, ch <-chan Message, within time.Duration) {
	t.Helper()
	select {
	case m := <-ch:
		t.Fatalf("unexpected message %q", m.Content)
	case <-time.After(within):
	}
}

func tmpFile(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "log.txt")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func appendFile(t *testing.T, path, content string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteString(content); err != nil {
		t.Fatal(err)
	}
}

func emptyRegistry(t *testing.T) *Registry {
	t.Helper()
	return LoadRegistry(filepath.Join(t.TempDir(), "registry.json"), discardLogger())
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}
