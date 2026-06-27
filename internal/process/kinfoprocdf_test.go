package process

import (
	"encoding/binary"
	"os"
	"testing"
)

// buildDragonFlyProc synthesises one struct kinfo_proc record (DragonFly amd64) with the
// fields the parser reads placed at their pinned offsets, so a decode mismatch is a real
// offset regression.
func buildDragonFlyProc(pid, ppid, uid, gid, nthreads, stat, lwpstat int32, comm string, vms, rssPages uint64, startSec, utSec, stSec int64, nvcsw, nivcsw uint64) []byte {
	r := make([]byte, dfStride)
	put32 := func(off int, v int32) { binary.LittleEndian.PutUint32(r[off:], uint32(v)) }
	put64 := func(off int, v uint64) { binary.LittleEndian.PutUint64(r[off:], v) }
	putTv := func(off int, sec int64) { // struct timeval{ int64 sec; int64 usec }
		binary.LittleEndian.PutUint64(r[off:], uint64(sec))
		binary.LittleEndian.PutUint64(r[off+8:], 0)
	}
	put32(dfPid, pid)
	put32(dfPpid, ppid)
	put32(dfRuid, uid)
	put32(dfRgid, gid)
	put32(dfNthreads, nthreads)
	put32(dfStat, stat)
	put32(dfLwpStat, lwpstat)
	copy(r[dfComm:dfComm+dfCommLen-1], comm)
	put64(dfVmMapSize, vms)
	put64(dfVmRssize, rssPages)
	putTv(dfStart, startSec)
	putTv(dfUtime, utSec)
	putTv(dfStime, stSec)
	put64(dfNvcsw, nvcsw)
	put64(dfNivcsw, nivcsw)
	return r
}

func TestParseDragonFlyProcsSynthetic(t *testing.T) {
	r1 := buildDragonFlyProc(4242, 1, 1001, 1001, 3, dfSACTIVE, dfLSRUN, "agent", 1<<30, 256, 1_700_000_000, 2, 1, 55, 7)
	r2 := buildDragonFlyProc(1, 0, 0, 0, 0, dfSACTIVE, dfLSSLEEP, "init", 2_000_000, 100, 1_699_999_000, 0, 0, 0, 0)
	procs := parseDragonFlyProcs(append(r1, r2...))
	if len(procs) != 2 {
		t.Fatalf("got %d procs, want 2", len(procs))
	}
	p := procs[0]
	if p.Pid != 4242 || p.Ppid != 1 || p.Uid != 1001 || p.Gid != 1001 {
		t.Errorf("ids: pid=%d ppid=%d uid=%d gid=%d", p.Pid, p.Ppid, p.Uid, p.Gid)
	}
	if p.NsPid != p.Pid {
		t.Errorf("NsPid=%d, want %d", p.NsPid, p.Pid)
	}
	if p.Name != "agent" {
		t.Errorf("Name=%q, want agent", p.Name)
	}
	if p.Threads != 3 {
		t.Errorf("Threads=%d, want 3", p.Threads)
	}
	if p.State != stateR { // SACTIVE + LSRUN
		t.Errorf("State=%d, want %d", p.State, stateR)
	}
	if p.Vms != 1<<30 {
		t.Errorf("Vms=%d, want %d", p.Vms, 1<<30)
	}
	if p.Rss != 256*pageSize {
		t.Errorf("Rss=%d, want %d", p.Rss, 256*pageSize)
	}
	if p.CreateTime != 1_700_000_000*1000 {
		t.Errorf("CreateTime=%d, want %d", p.CreateTime, 1_700_000_000*1000)
	}
	if p.UserTime != 2 || p.SystemTime != 1 {
		t.Errorf("cpu: user=%v system=%v, want 2 1", p.UserTime, p.SystemTime)
	}
	if p.VoluntaryCtxSwitches != 55 || p.InvoluntaryCtxSwitches != 7 {
		t.Errorf("ctxsw: nv=%d niv=%d, want 55 7", p.VoluntaryCtxSwitches, p.InvoluntaryCtxSwitches)
	}
	// init reported 0 threads, which the parser lifts to 1 so the intake keeps it, and its
	// SACTIVE + LSSLEEP maps to sleeping.
	if procs[1].Threads != 1 {
		t.Errorf("init Threads=%d, want 1 (zero lifted)", procs[1].Threads)
	}
	if procs[1].State != stateS {
		t.Errorf("init State=%d, want %d", procs[1].State, stateS)
	}
}

func TestParseDragonFlyProcsTruncated(t *testing.T) {
	blob := buildDragonFlyProc(2, 1, 0, 0, 1, dfSACTIVE, dfLSSLEEP, "a", 1, 1, 1, 0, 0, 0, 0)
	blob = append(blob, make([]byte, dfStride-8)...) // a short trailing record
	if procs := parseDragonFlyProcs(blob); len(procs) != 1 {
		t.Fatalf("got %d procs, want 1 (partial trailing record dropped)", len(procs))
	}
}

func TestDragonflyState(t *testing.T) {
	cases := []struct {
		stat, lwp int32
		want      ProcState
	}{
		{dfSIDL, 0, stateD},
		{dfSSTOP, 0, stateT},
		{dfSZOMB, 0, stateZ},
		{dfSCORE, 0, stateR},
		{dfSACTIVE, dfLSRUN, stateR},
		{dfSACTIVE, dfLSSLEEP, stateS},
		{dfSACTIVE, dfLSSTOP, stateT},
		{99, 0, stateU},
	}
	for _, c := range cases {
		r := make([]byte, dfStride)
		binary.LittleEndian.PutUint32(r[dfStat:], uint32(c.stat))
		binary.LittleEndian.PutUint32(r[dfLwpStat:], uint32(c.lwp))
		if got := dragonflyState(r); got != c.want {
			t.Errorf("stat=%d lwp=%d: got %d, want %d", c.stat, c.lwp, got, c.want)
		}
	}
}

// TestParseDragonFlyProcsGolden decodes a real kern.proc.all blob captured from DragonFly
// 6.4.2 amd64 by the vm_dragonfly e2e, the ground truth for the pinned offsets.
func TestParseDragonFlyProcsGolden(t *testing.T) {
	b, err := os.ReadFile("testdata/dragonfly_procall.bin")
	if err != nil {
		t.Fatal(err)
	}
	procs := parseDragonFlyProcs(b)
	if len(procs) != len(b)/dfStride || len(procs) == 0 {
		t.Fatalf("got %d procs from %d bytes (stride %d)", len(procs), len(b), dfStride)
	}
	byName := map[string]Proc{}
	for _, p := range procs {
		if p.Pid < 0 {
			t.Errorf("negative pid %d (%q)", p.Pid, p.Name)
		}
		if p.Threads < 1 {
			t.Errorf("pid %d (%q) has Threads=%d, the intake would drop it", p.Pid, p.Name, p.Threads)
		}
		if p.Name == "" {
			t.Errorf("pid %d has an empty name", p.Pid)
		}
		byName[p.Name] = p
	}
	// pid 1 is init, its parent is the pid-0 kernel process, and it has a real address space.
	init := procs[1]
	if init.Pid != 1 || init.Name != "init" || init.Ppid != 0 {
		t.Errorf("procs[1] = pid %d %q ppid %d, want pid 1 init ppid 0", init.Pid, init.Name, init.Ppid)
	}
	if init.Vms == 0 || init.Rss == 0 || init.CreateTime <= 0 {
		t.Errorf("init vms=%d rss=%d ctime=%d, want all positive", init.Vms, init.Rss, init.CreateTime)
	}
	for _, name := range []string{"init", "syslogd", "getty"} {
		if _, ok := byName[name]; !ok {
			t.Errorf("expected a %q process in the captured table", name)
		}
	}
}
