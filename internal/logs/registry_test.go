package logs

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRegistryRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "registry.json")
	r := LoadRegistry(path, nil)
	r.Advance("file:/var/log/a.log", 100)
	r.Advance("file:/var/log/b.log", 250)
	r.Flush()

	r2 := LoadRegistry(path, nil)
	if got := r2.Offset("file:/var/log/a.log"); got != 100 {
		t.Errorf("a offset = %d, want 100", got)
	}
	if got := r2.Offset("file:/var/log/b.log"); got != 250 {
		t.Errorf("b offset = %d, want 250", got)
	}
}

func TestRegistryCursorRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "registry.json")
	r := LoadRegistry(path, nil)
	r.Advance("file:/var/log/a.log", 100) // a file offset and a journald cursor coexist
	r.AdvanceCursor("journald:default", "s=abc;i=1;b=2")
	r.AdvanceCursor("journald:default", "s=abc;i=9;b=2") // later read wins
	r.Flush()

	r2 := LoadRegistry(path, nil)
	if got := r2.Offset("file:/var/log/a.log"); got != 100 {
		t.Errorf("file offset = %d, want 100", got)
	}
	cur, ok := r2.PositionCursor("journald:default")
	if !ok || cur != "s=abc;i=9;b=2" {
		t.Errorf("cursor = %q (ok=%v), want %q", cur, ok, "s=abc;i=9;b=2")
	}
	if _, ok := r2.PositionCursor("journald:unseen"); ok {
		t.Error("unseen journald source reported a cursor")
	}
}

// Advance follows the acknowledgements wherever they go: the single ordered
// batcher makes the last write the furthest delivered byte, and after a rotation
// the new file's low offsets must stick, or a restart would seek the new file to
// the old file's offset and skip undelivered bytes.
func TestRegistryAdvanceFollowsAcks(t *testing.T) {
	r := LoadRegistry(filepath.Join(t.TempDir(), "r.json"), nil)
	r.Advance("id", 100)
	r.Advance("id", 50) // rotation: the new file acks at low offsets again
	if got := r.Offset("id"); got != 50 {
		t.Errorf("offset = %d, want 50 (a rotation rewind must stick)", got)
	}
}

// The on-disk format is pinned byte for byte: Version 2, offsets as decimal
// strings, cursors verbatim. Loading an existing registry after an upgrade
// depends on these exact bytes.
func TestRegistryGoldenFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "registry.json")
	r := LoadRegistry(path, nil)
	r.Advance("file:/var/log/a.log", 100)
	r.AdvanceCursor("journald:default", "s=abc;i=9")
	r.Flush()

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"Version":2,"Registry":{"file:/var/log/a.log":{"Offset":"100"},"journald:default":{"Cursor":"s=abc;i=9"}}}`
	if string(got) != want {
		t.Errorf("registry.json = %s, want %s", got, want)
	}
}

func TestRegistryRemove(t *testing.T) {
	r := LoadRegistry(filepath.Join(t.TempDir(), "r.json"), nil)
	r.Advance("file:/var/log/gone.log", 100)
	r.Remove("file:/var/log/gone.log")
	if _, ok := r.Position("file:/var/log/gone.log"); ok {
		t.Error("removed entry still present")
	}
}

func TestRegistryMissingFile(t *testing.T) {
	r := LoadRegistry(filepath.Join(t.TempDir(), "absent.json"), nil)
	if got := r.Offset("anything"); got != 0 {
		t.Errorf("offset = %d, want 0", got)
	}
}

func TestRegistryCorruptFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "r.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	r := LoadRegistry(path, nil) // must not panic
	if got := r.Offset("id"); got != 0 {
		t.Errorf("offset = %d, want 0 on corrupt file", got)
	}
}

func TestRegistryFlushIsAtomic(t *testing.T) {
	path := filepath.Join(t.TempDir(), "r.json")
	r := LoadRegistry(path, nil)
	r.Advance("id", 7)
	r.Flush()

	if _, err := os.Stat(path); err != nil {
		t.Errorf("registry not written: %v", err)
	}
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Errorf("temp file left behind: %v", err)
	}
}
