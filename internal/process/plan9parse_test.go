package process

import (
	"fmt"
	"testing"
)

// statusLine builds a /proc/<pid>/status line exactly as devproc.c writes it: name
// and user space-padded to KNAMELEN (28), state to 12, then each number
// right-justified in a 12-byte column (readnum's "%11lud " shape).
func statusLine(name, user, state string, nums ...uint64) string {
	line := fmt.Sprintf("%-28s%-28s%-12s", name, user, state)
	for _, n := range nums {
		line += fmt.Sprintf("%11d ", n)
	}
	return line
}

func TestParsePlan9Status(t *testing.T) {
	// Stock kernel: t0..t5 mem basepri pri (nine numbers).
	data := statusLine("init", "eve", "Wakeme", 100, 50, 999999, 0, 0, 0, 2048, 10, 10)
	st, ok := parsePlan9Status(data)
	if !ok {
		t.Fatal("parsePlan9Status returned !ok")
	}
	if st.name != "init" || st.user != "eve" || st.state != "Wakeme" ||
		st.utimeMs != 100 || st.stimeMs != 50 || st.realMs != 999999 || st.memKB != 2048 {
		t.Errorf("parsePlan9Status = %+v", st)
	}
	// A program name or user containing a space stays intact: the text columns
	// are fixed width, so it cannot shift the later fields.
	spaced := statusLine("factotum helper", "spaced user", "Rendez", 1, 2, 3, 0, 0, 0, 512, 10, 10)
	if st, ok := parsePlan9Status(spaced); !ok || st.name != "factotum helper" ||
		st.user != "spaced user" || st.state != "Rendez" || st.memKB != 512 {
		t.Errorf("spaced name = %+v ok=%v", st, ok)
	}
	if _, ok := parsePlan9Status("too few fields"); ok {
		t.Error("parsePlan9Status accepted a short line")
	}
}

// TestParsePlan9StatusExtended covers the nix kernel lineage, which appends three
// counters (ntrap nintr nsyscall) after priority (twelve numbers, not nine).
// Memory must still come from the seventh number. A right anchor lands on ntrap
// and, when that is zero, drops the process's memory from the payload.
func TestParsePlan9StatusExtended(t *testing.T) {
	// ntrap (the tenth number) is 7 here, distinct from mem (4096) to catch a
	// right anchor.
	data := statusLine("httpd", "glenda", "Fault", 100, 50, 999999, 0, 0, 0, 4096, 10, 10, 7, 3, 21)
	st, ok := parsePlan9Status(data)
	if !ok {
		t.Fatal("parsePlan9Status returned !ok")
	}
	if st.memKB != 4096 {
		t.Errorf("memKB = %d, want 4096 (a right anchor would give ntrap=7)", st.memKB)
	}
	if st.name != "httpd" || st.user != "glenda" || st.state != "Fault" ||
		st.utimeMs != 100 || st.stimeMs != 50 || st.realMs != 999999 {
		t.Errorf("parsePlan9Status = %+v", st)
	}
}

func TestParsePlan9MemTotal(t *testing.T) {
	// /dev/swap: the "memory" line carries total RAM in bytes. The pagesize and
	// used/total "user" lines and the multi-word kernel pools must not match.
	const data = "2147483648 memory\n" +
		"4096 pagesize\n" +
		"15360/25600 user\n" +
		"12345/67890 kernel malloc\n"
	if got := parsePlan9MemTotal(data); got != 2147483648 {
		t.Errorf("parsePlan9MemTotal = %d, want 2147483648", got)
	}
	if got := parsePlan9MemTotal("no memory line here\n"); got != 0 {
		t.Errorf("parsePlan9MemTotal(none) = %d, want 0", got)
	}
}

func TestParsePlan9Shared(t *testing.T) {
	// /proc/<pid>/segment: "type perms base top ref". The perms column (an R and/or
	// P, blank when neither) varies the field count, so parsing is right-anchored.
	// Only ref > 1 (mapped into more than one process) is shared. This mixes the 386
	// kernel's zero-padded %.8lux with a bare-hex value to exercise both forms.
	const data = "Text   R  00001000 00002000    3\n" + // shared text, 0x1000 = 4096
		"Data     00010000 00014000    2\n" + // shared data, 0x4000 = 16384
		"Bss      00020000 00021000    1\n" + // private (ref 1), excluded
		"Stack    7ffffff000 7fffffffff    1\n" // private, excluded
	if got, want := parsePlan9Shared(data), uint64(4096+16384); got != want {
		t.Errorf("parsePlan9Shared = %d, want %d", got, want)
	}
	// amd64 kernel's %p form: bare hex, no padding, blank perms column collapsed.
	if got, want := parsePlan9Shared("Data 201000 211000 4\n"), uint64(0x10000); got != want {
		t.Errorf("parsePlan9Shared(amd64) = %d, want %d", got, want)
	}
	if got := parsePlan9Shared("Data 00010000 00014000 1\n"); got != 0 {
		t.Errorf("parsePlan9Shared(private only) = %d, want 0", got)
	}
	if got := parsePlan9Shared(""); got != 0 {
		t.Errorf("parsePlan9Shared(empty) = %d, want 0", got)
	}
}

func TestPlan9State(t *testing.T) {
	cases := map[string]ProcState{
		// On/bound for a CPU (statename[] in port/proc.c).
		"Running": stateR, "Ready": stateR, "Scheding": stateR, "New": stateR,
		// Stopped / dead / broken.
		"Stopped": stateT, "Moribund": stateZ, "Dead": stateZ, "Broken": stateX,
		// Blocked: the Queueing* qlock waits are descheduled, not runnable.
		"Queueing": stateS, "QueueingR": stateS, "QueueingW": stateS,
		// Other scheduler sleeps.
		"Wakeme": stateS, "Rendez": stateS, "Waitrelease": stateS,
		// psstate wait-channel names (and anything unrecognised) read as sleeping.
		"I/O": stateS, "Pageout": stateS, "Fault": stateS, "Bogus": stateS,
	}
	for s, want := range cases {
		if got := plan9State(s); got != want {
			t.Errorf("plan9State(%q) = %d, want %d", s, got, want)
		}
	}
}

func TestParsePlan9Fds(t *testing.T) {
	// /proc/<pid>/fd: cwd on line 1, then one line per open fd.
	const data = "/usr/glenda\n" +
		"   0 r  M    9 (0000000000000001 0 00)     8192        0 /dev/cons\n" +
		"   1 w  M    9 (0000000000000001 0 00)     8192        0 /dev/cons\n" +
		"   2 w  M    9 (0000000000000001 0 00)     8192        0 /dev/cons\n"
	cwd, nfd := parsePlan9Fds(data)
	if cwd != "/usr/glenda" || nfd != 3 {
		t.Errorf("parsePlan9Fds = %q, %d; want /usr/glenda, 3", cwd, nfd)
	}
	if cwd, nfd := parsePlan9Fds("/\n"); cwd != "/" || nfd != 0 {
		t.Errorf("cwd-only = %q, %d; want /, 0", cwd, nfd)
	}
	if cwd, nfd := parsePlan9Fds(""); cwd != "" || nfd != 0 {
		t.Errorf("empty = %q, %d; want \"\", 0", cwd, nfd)
	}
}

func TestParsePlan9Args(t *testing.T) {
	got := parsePlan9Args("/bin/rc -l\n")
	if len(got) != 2 || got[0] != "/bin/rc" || got[1] != "-l" {
		t.Errorf("parsePlan9Args = %q", got)
	}
}
