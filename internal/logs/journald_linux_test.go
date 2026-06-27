package logs

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/0intro/dd-micro-agent/internal/config"
)

// A failing journalctl respawns at the tailer's pace, never in a tight fork
// loop, and every spawn resumes from the stored cursor.
func TestJournaldRespawnThrottled(t *testing.T) {
	dir := t.TempDir()
	argsLog := filepath.Join(dir, "args.log")
	script := "#!/bin/sh\necho \"$@\" >> " + argsLog + "\nexit 1\n"
	if err := os.WriteFile(filepath.Join(dir, "journalctl"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	reg := emptyRegistry(t)
	reg.AdvanceCursor("journald:default", "s=abc;i=7")

	tl := &journaldTailer{
		source:   config.LogSource{Type: "journald"},
		id:       "journald:default",
		out:      make(chan Message, 4),
		registry: reg,
		pause:    20 * time.Millisecond,
		log:      discardLogger(),
	}
	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()
	done := make(chan struct{})
	go func() { tl.run(ctx); close(done) }()
	<-done

	data, err := os.ReadFile(argsLog)
	if err != nil {
		t.Fatal(err)
	}
	spawns := strings.Split(strings.TrimSpace(string(data)), "\n")
	if n := len(spawns); n < 2 || n > 20 {
		t.Errorf("journalctl spawned %d times in 250ms at a 20ms pause, want a handful (a fork loop spawns hundreds)", n)
	}
	for i, args := range spawns {
		if !strings.Contains(args, "--after-cursor=s=abc;i=7") {
			t.Errorf("spawn %d args %q missing the stored cursor", i, args)
		}
	}
}
