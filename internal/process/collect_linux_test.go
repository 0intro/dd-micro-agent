//go:build linux

package process

import (
	"os"
	"testing"
)

func TestParseStat(t *testing.T) {
	// comm "a)b" contains a ')': it must be taken between the first '(' and the
	// LAST ')'. 22 numeric fields follow (state = field 3 = index 0).
	const data = "1234 (a)b) S 1 1234 1234 0 -1 4194560 1000 0 50 0 12 5 0 0 20 0 7 0 9999 123456789 543\n"
	st, ok := parseStat(data)
	if !ok {
		t.Fatal("parseStat returned !ok")
	}
	if st.comm != "a)b" {
		t.Errorf("comm = %q, want a)b", st.comm)
	}
	if st.state != 'S' || st.ppid != 1 || st.utime != 12 || st.stime != 5 ||
		st.numThreads != 7 || st.starttime != 9999 || st.vsize != 123456789 || st.rssPages != 543 {
		t.Errorf("parseStat = %+v", st)
	}
	if _, ok := parseStat("1 (noFields)"); ok {
		t.Error("parseStat accepted a line with too few fields")
	}
}

func TestParseStatus(t *testing.T) {
	const data = "Name:\tmyproc\n" +
		"State:\tS (sleeping)\n" +
		"Uid:\t1000\t1000\t1000\t1000\n" +
		"Gid:\t1001\t1001\t1001\t1001\n" +
		"VmSize:\t  123456 kB\n" +
		"VmRSS:\t   4096 kB\n" +
		"VmSwap:\t      16 kB\n" +
		"Threads:\t7\n" +
		"voluntary_ctxt_switches:\t100\n" +
		"nonvoluntary_ctxt_switches:\t5\n"
	s := parseStatus(data)
	if !s.haveUID || s.uid != 1000 || s.gid != 1001 || s.vmRSS != 4096 || s.vmSize != 123456 ||
		s.vmSwap != 16 || s.threads != 7 || s.volctx != 100 || s.nonvolctx != 5 {
		t.Errorf("parseStatus = %+v", s)
	}
}

func TestParseCmdline(t *testing.T) {
	got := parseCmdline([]byte("/usr/bin/agent\x00-debug\x00\x00"))
	if len(got) != 2 || got[0] != "/usr/bin/agent" || got[1] != "-debug" {
		t.Errorf("parseCmdline = %q", got)
	}
	if got := parseCmdline(nil); got != nil {
		t.Errorf("parseCmdline(nil) = %q, want nil", got)
	}
}

func TestParseIO(t *testing.T) {
	const data = "rchar: 100\nwchar: 200\nsyscr: 10\nsyscw: 20\nread_bytes: 4096\nwrite_bytes: 8192\n"
	var p Proc
	parseIO(data, &p)
	if p.ReadCount != 10 || p.WriteCount != 20 || p.ReadBytes != 4096 || p.WriteBytes != 8192 {
		t.Errorf("parseIO = %+v", p)
	}
}

func TestLinuxState(t *testing.T) {
	cases := map[byte]ProcState{'R': stateR, 'S': stateS, 'I': stateS, 'D': stateD, 'Z': stateZ, 'T': stateT, '?': stateU}
	for c, want := range cases {
		if got := linuxState(c); got != want {
			t.Errorf("linuxState(%q) = %d, want %d", c, got, want)
		}
	}
}

func TestHostTotalMemorySmoke(t *testing.T) {
	if got := hostTotalMemory(); got <= 0 {
		t.Errorf("hostTotalMemory() = %d, want > 0 (real /proc/meminfo)", got)
	}
}

func TestCollectProcsSmoke(t *testing.T) {
	procs, err := collectProcs()
	if err != nil {
		t.Fatal(err)
	}
	me := int32(os.Getpid())
	for _, p := range procs {
		if p.Pid == me {
			if p.Name == "" {
				t.Error("own process has empty Name")
			}
			if p.State != stateR && p.State != stateS {
				t.Errorf("own process state = %d, want running or sleeping", p.State)
			}
			return
		}
	}
	t.Errorf("did not find own pid %d among %d processes", me, len(procs))
}
