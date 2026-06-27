package hostname

import (
	"os"
	"path/filepath"
	"testing"
)

// Only the pure precedence paths are tested: the cloud fallback would make the
// result depend on where the test runs (a CI runner is itself a cloud VM).
func TestResolve(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hostname")
	if err := os.WriteFile(path, []byte("  filehost\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if got := Resolve("conf", path); got != "conf" {
		t.Errorf("configured hostname = %q, want conf (it wins over the file)", got)
	}
	if got := Resolve("", path); got != "filehost" {
		t.Errorf("file hostname = %q, want filehost (whitespace trimmed)", got)
	}
	if got := Resolve("conf", filepath.Join(t.TempDir(), "absent")); got != "conf" {
		t.Errorf("hostname = %q, want conf (a missing file is not an error)", got)
	}
}

func TestIsDefaultHostname(t *testing.T) {
	tests := map[string]bool{
		"":                      true,
		"localhost":             true,
		"LOCALHOST":             true,
		"localhost.localdomain": true,
		"ip-10-0-0-1":           true,
		"IP-10-0-0-1":           true,
		"domU-12-31-39-00-00":   true,
		"EC2AMAZ-ABC123":        true,
		"web1":                  false,
		"ipswitch":              false, // "ip" alone is not the prefix, "ip-" is
		"my-ip-box":             false,
	}
	for h, want := range tests {
		if got := isDefaultHostname(h); got != want {
			t.Errorf("isDefaultHostname(%q) = %v, want %v", h, got, want)
		}
	}
}
