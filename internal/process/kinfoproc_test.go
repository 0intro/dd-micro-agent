package process

// Self-contained (no freebsd-only helpers) so it runs on the dev host. Records are
// synthesized at the offsets parseKinfoProcs reads. Those offsets are pinned for the
// amd64 ABI and verified against a real kern.proc.all blob by the vm_freebsd e2e.

import (
	"encoding/binary"
	"testing"
)

// realStructsize is sizeof(struct kinfo_proc) on FreeBSD 12 to 15 amd64.
const realStructsize = 1088

type kinfoFields struct {
	structsize          int32
	pid, ppid           int32
	ruid, rgid          uint32
	size                uint64 // ki_size (Vms bytes)
	rssize              int64  // ki_rssize (pages)
	startSec, startUsec int64
	stat                byte
	comm                string
	numthreads          int32
	utimeSec, utimeUsec int64
	stimeSec, stimeUsec int64
	nvcsw, nivcsw       uint64
}

// buildKinfoProc renders one struct kinfo_proc. The backing buffer is always at least
// a full record so writes can't run off the end even when structsize is set small to
// exercise a guard.
func buildKinfoProc(f kinfoFields) []byte {
	n := int(f.structsize)
	if n < realStructsize {
		n = realStructsize
	}
	r := make([]byte, n)
	binary.LittleEndian.PutUint32(r[kpStructsize:], uint32(f.structsize))
	binary.LittleEndian.PutUint32(r[kpPid:], uint32(f.pid))
	binary.LittleEndian.PutUint32(r[kpPpid:], uint32(f.ppid))
	binary.LittleEndian.PutUint32(r[kpRuid:], f.ruid)
	binary.LittleEndian.PutUint32(r[kpRgid:], f.rgid)
	binary.LittleEndian.PutUint64(r[kpSize:], f.size)
	binary.LittleEndian.PutUint64(r[kpRssize:], uint64(f.rssize))
	putTimeval(r, kpStart, f.startSec, f.startUsec)
	r[kpStat] = f.stat
	copy(r[kpComm:kpComm+kpCommLen], f.comm)
	binary.LittleEndian.PutUint32(r[kpNumthreads:], uint32(f.numthreads))
	putTimeval(r, kpUtime, f.utimeSec, f.utimeUsec)
	putTimeval(r, kpStime, f.stimeSec, f.stimeUsec)
	binary.LittleEndian.PutUint64(r[kpNvcsw:], f.nvcsw)
	binary.LittleEndian.PutUint64(r[kpNivcsw:], f.nivcsw)
	return r
}

func putTimeval(r []byte, off int, sec, usec int64) {
	binary.LittleEndian.PutUint64(r[off:], uint64(sec))
	binary.LittleEndian.PutUint64(r[off+8:], uint64(usec))
}

func TestParseKinfoProcs(t *testing.T) {
	blob := buildKinfoProc(kinfoFields{
		structsize: realStructsize,
		pid:        4242, ppid: 1,
		ruid: 1001, rgid: 1002,
		size:     1 << 32, // 4 GiB virtual
		rssize:   2000,    // pages -> 2000*4096 bytes
		startSec: 1_700_000_000, startUsec: 500_000,
		stat: sRUN, comm: "agent", numthreads: 7,
		utimeSec: 12, utimeUsec: 500_000, // 12.5s
		stimeSec: 3, stimeUsec: 250_000, // 3.25s
		nvcsw: 111, nivcsw: 222,
	})

	got := parseKinfoProcs(blob)
	if len(got) != 1 {
		t.Fatalf("len = %d, want 1", len(got))
	}
	p := got[0]
	checks := []struct {
		name string
		got  any
		want any
	}{
		{"Pid", p.Pid, int32(4242)},
		{"NsPid", p.NsPid, int32(4242)},
		{"Ppid", p.Ppid, int32(1)},
		{"Uid", p.Uid, int32(1001)},
		{"Gid", p.Gid, int32(1002)},
		{"Name", p.Name, "agent"},
		{"Exe", p.Exe, ""}, // filled by collect_freebsd, not the parser
		{"State", p.State, stateR},
		{"Threads", p.Threads, int32(7)},
		{"Vms", p.Vms, uint64(1) << 32},
		{"Rss", p.Rss, uint64(2000) * pageSize},
		{"CreateTime", p.CreateTime, int64(1_700_000_000_500)},
		{"UserTime", p.UserTime, 12.5},
		{"SystemTime", p.SystemTime, 3.25},
		{"VoluntaryCtxSwitches", p.VoluntaryCtxSwitches, uint64(111)},
		{"InvoluntaryCtxSwitches", p.InvoluntaryCtxSwitches, uint64(222)},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s = %v, want %v", c.name, c.got, c.want)
		}
	}
}

func TestParseKinfoProcsMultiple(t *testing.T) {
	a := buildKinfoProc(kinfoFields{structsize: realStructsize, pid: 10, stat: sRUN, comm: "a", numthreads: 1})
	b := buildKinfoProc(kinfoFields{structsize: realStructsize, pid: 20, stat: sSLEEP, comm: "b", numthreads: 1})
	got := parseKinfoProcs(append(a, b...))
	if len(got) != 2 || got[0].Pid != 10 || got[1].Pid != 20 {
		t.Fatalf("got %d procs (%v), want pids [10 20]", len(got), pids(got))
	}
}

// A trailing record whose declared size overruns the buffer is dropped, not decoded
// from out-of-bounds memory. The records before it still parse.
func TestParseKinfoProcsTruncatedTail(t *testing.T) {
	full := buildKinfoProc(kinfoFields{structsize: realStructsize, pid: 10, stat: sRUN, comm: "a", numthreads: 1})
	tail := buildKinfoProc(kinfoFields{structsize: realStructsize, pid: 20, stat: sRUN, comm: "b", numthreads: 1})
	blob := append(full, tail[:700]...) // second record claims 1088 bytes but only 700 are present
	got := parseKinfoProcs(blob)
	if len(got) != 1 || got[0].Pid != 10 {
		t.Fatalf("got %v, want pid [10] only", pids(got))
	}
}

func TestParseKinfoProcsBadStructsize(t *testing.T) {
	for _, ss := range []int32{0, -1, kpMinSize - 1} {
		blob := buildKinfoProc(kinfoFields{structsize: ss, pid: 7, comm: "x"})
		if got := parseKinfoProcs(blob); len(got) != 0 {
			t.Errorf("structsize=%d: got %d procs, want 0 (guard should break)", ss, len(got))
		}
	}
}

func TestFreebsdState(t *testing.T) {
	cases := map[byte]ProcState{
		sIDL: stateD, sRUN: stateR, sSLEEP: stateS, sSTOP: stateT,
		sZOMB: stateZ, sWAIT: stateS, sLOCK: stateD, 99: stateU,
	}
	for in, want := range cases {
		if got := freebsdState(in); got != want {
			t.Errorf("freebsdState(%d) = %d, want %d", in, got, want)
		}
	}
}

func TestParseArgv(t *testing.T) {
	if got := parseArgv([]byte("/usr/local/bin/agent\x00--debug\x00\x00")); len(got) != 2 ||
		got[0] != "/usr/local/bin/agent" || got[1] != "--debug" {
		t.Errorf("parseArgv = %q, want [/usr/local/bin/agent --debug]", got)
	}
	if got := parseArgv(nil); got != nil {
		t.Errorf("parseArgv(nil) = %q, want nil", got)
	}
}

func pids(ps []Proc) []int32 {
	out := make([]int32, len(ps))
	for i, p := range ps {
		out[i] = p.Pid
	}
	return out
}
