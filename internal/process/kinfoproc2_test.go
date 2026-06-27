package process

// Self-contained tests for the OpenBSD/NetBSD parsers. Records are synthesized at
// the offsets the parsers read. Those offsets are pinned for the amd64 ABI (OpenBSD
// from offsetof on a 7.9 VM, NetBSD from the netbsd-10 struct) and verified against a
// live blob plus ps by the vm_openbsd and vm_netbsd e2e tests.

import (
	"encoding/binary"
	"reflect"
	"testing"
)

type obFields struct {
	pid, ppid           int32
	ruid, rgid          uint32
	stat                int8
	comm                string
	rssize              int32 // pages
	tsize, dsize, ssize int32 // pages
	startSec, startUsec uint64
	utimeSec, utimeUsec uint32
	stimeSec, stimeUsec uint32
	nvcsw, nivcsw       uint64
}

func buildOpenBSDProc(f obFields) []byte {
	r := make([]byte, obStride)
	binary.LittleEndian.PutUint32(r[obPid:], uint32(f.pid))
	binary.LittleEndian.PutUint32(r[obPpid:], uint32(f.ppid))
	binary.LittleEndian.PutUint32(r[obRuid:], f.ruid)
	binary.LittleEndian.PutUint32(r[obRgid:], f.rgid)
	r[obStat] = byte(f.stat)
	copy(r[obComm:obComm+obComLen], f.comm)
	binary.LittleEndian.PutUint32(r[obRssize:], uint32(f.rssize))
	binary.LittleEndian.PutUint32(r[obTsize:], uint32(f.tsize))
	binary.LittleEndian.PutUint32(r[obDsize:], uint32(f.dsize))
	binary.LittleEndian.PutUint32(r[obSsize:], uint32(f.ssize))
	binary.LittleEndian.PutUint64(r[obStartSec:], f.startSec)
	binary.LittleEndian.PutUint32(r[obStartUsec:], uint32(f.startUsec))
	binary.LittleEndian.PutUint32(r[obUtimeSec:], f.utimeSec)
	binary.LittleEndian.PutUint32(r[obUtimeUsec:], f.utimeUsec)
	binary.LittleEndian.PutUint32(r[obStimeSec:], f.stimeSec)
	binary.LittleEndian.PutUint32(r[obStimeUsec:], f.stimeUsec)
	binary.LittleEndian.PutUint64(r[obNvcsw:], f.nvcsw)
	binary.LittleEndian.PutUint64(r[obNivcsw:], f.nivcsw)
	return r
}

func TestParseOpenBSDProcs(t *testing.T) {
	blob := buildOpenBSDProc(obFields{
		pid: 4242, ppid: 1, ruid: 1001, rgid: 1002,
		stat: obSRUN, comm: "agent",
		rssize: 2000, tsize: 100, dsize: 800, ssize: 124,
		startSec: 1_700_000_000, startUsec: 500_000,
		utimeSec: 12, utimeUsec: 500_000,
		stimeSec: 3, stimeUsec: 250_000,
		nvcsw: 111, nivcsw: 222,
	})
	got := parseOpenBSDProcs(blob)
	if len(got) != 1 {
		t.Fatalf("len = %d, want 1", len(got))
	}
	p := got[0]
	for _, c := range []struct {
		name      string
		got, want any
	}{
		{"Pid", p.Pid, int32(4242)},
		{"NsPid", p.NsPid, int32(4242)},
		{"Ppid", p.Ppid, int32(1)},
		{"Uid", p.Uid, int32(1001)},
		{"Gid", p.Gid, int32(1002)},
		{"Name", p.Name, "agent"},
		{"State", p.State, stateR},
		{"Threads", p.Threads, int32(1)},
		{"Vms", p.Vms, uint64(1024) * pageSize}, // text+data+stack pages, ps VSZ
		{"Rss", p.Rss, uint64(2000) * pageSize},
		{"CreateTime", p.CreateTime, int64(1_700_000_000_500)},
		{"UserTime", p.UserTime, 12.5},
		{"SystemTime", p.SystemTime, 3.25},
		{"VoluntaryCtxSwitches", p.VoluntaryCtxSwitches, uint64(111)},
		{"InvoluntaryCtxSwitches", p.InvoluntaryCtxSwitches, uint64(222)},
	} {
		if c.got != c.want {
			t.Errorf("%s = %v, want %v", c.name, c.got, c.want)
		}
	}
}

func TestParseOpenBSDArgv(t *testing.T) {
	// KERN_PROC_ARGV: an argv pointer vector, NULL-terminated, then the strings.
	ptr := func(v uint64) []byte {
		b := make([]byte, 8)
		binary.LittleEndian.PutUint64(b, v)
		return b
	}
	var buf []byte
	buf = append(buf, ptr(0x7f0000001000)...)
	buf = append(buf, ptr(0x7f0000001008)...)
	buf = append(buf, ptr(0)...)
	buf = append(buf, []byte("/bin/ls\x00-l\x00")...)

	if got, want := parseOpenBSDArgv(buf), []string{"/bin/ls", "-l"}; !reflect.DeepEqual(got, want) {
		t.Errorf("argv = %q, want %q", got, want)
	}
	if got := parseOpenBSDArgv(nil); got != nil {
		t.Errorf("empty buffer argv = %q, want nil", got)
	}
	if got := parseOpenBSDArgv(ptr(0x7f0000001000)); got != nil {
		t.Errorf("unterminated pointer vector argv = %q, want nil", got)
	}
}

type nbFields struct {
	pid, ppid           int32
	ruid, rgid          uint32
	stat                int8
	comm                string
	rssize              int32 // pages
	vsize               int64 // pages
	nlwps               uint64
	startSec, startUsec uint32
	utimeSec, utimeUsec uint32
	stimeSec, stimeUsec uint32
	nvcsw, nivcsw       uint64
}

func buildNetBSDProc(f nbFields) []byte {
	r := make([]byte, nbStride)
	binary.LittleEndian.PutUint32(r[nbPid:], uint32(f.pid))
	binary.LittleEndian.PutUint32(r[nbPpid:], uint32(f.ppid))
	binary.LittleEndian.PutUint32(r[nbRuid:], f.ruid)
	binary.LittleEndian.PutUint32(r[nbRgid:], f.rgid)
	r[nbStat] = byte(f.stat)
	copy(r[nbComm:nbComm+nbComLen], f.comm)
	binary.LittleEndian.PutUint32(r[nbRssize:], uint32(f.rssize))
	binary.LittleEndian.PutUint64(r[nbVsize:], uint64(f.vsize))
	binary.LittleEndian.PutUint64(r[nbNlwps:], f.nlwps)
	binary.LittleEndian.PutUint32(r[nbStartSec:], f.startSec)
	binary.LittleEndian.PutUint32(r[nbStartUsec:], f.startUsec)
	binary.LittleEndian.PutUint32(r[nbUtimeSec:], f.utimeSec)
	binary.LittleEndian.PutUint32(r[nbUtimeUsec:], f.utimeUsec)
	binary.LittleEndian.PutUint32(r[nbStimeSec:], f.stimeSec)
	binary.LittleEndian.PutUint32(r[nbStimeUsec:], f.stimeUsec)
	binary.LittleEndian.PutUint64(r[nbNvcsw:], f.nvcsw)
	binary.LittleEndian.PutUint64(r[nbNivcsw:], f.nivcsw)
	return r
}

func TestParseNetBSDProcs(t *testing.T) {
	blob := buildNetBSDProc(nbFields{
		pid: 909, ppid: 1, ruid: 0, rgid: 0,
		stat: nbLSSLEEP, comm: "init",
		rssize: 100, vsize: 1 << 20, nlwps: 4,
		startSec: 1_700_000_000, startUsec: 250_000,
		utimeSec: 1, utimeUsec: 0,
		stimeSec: 0, stimeUsec: 500_000,
		nvcsw: 5, nivcsw: 6,
	})
	got := parseNetBSDProcs(blob)
	if len(got) != 1 {
		t.Fatalf("len = %d, want 1", len(got))
	}
	p := got[0]
	for _, c := range []struct {
		name      string
		got, want any
	}{
		{"Pid", p.Pid, int32(909)},
		{"Ppid", p.Ppid, int32(1)},
		{"Uid", p.Uid, int32(0)},
		{"Name", p.Name, "init"},
		{"State", p.State, stateS},
		{"Threads", p.Threads, int32(4)},
		{"Rss", p.Rss, uint64(100) * pageSize},
		{"Vms", p.Vms, uint64(1<<20) * pageSize},
		{"CreateTime", p.CreateTime, int64(1_700_000_000_250)},
		{"UserTime", p.UserTime, 1.0},
		{"SystemTime", p.SystemTime, 0.5},
		{"VoluntaryCtxSwitches", p.VoluntaryCtxSwitches, uint64(5)},
		{"InvoluntaryCtxSwitches", p.InvoluntaryCtxSwitches, uint64(6)},
	} {
		if c.got != c.want {
			t.Errorf("%s = %v, want %v", c.name, c.got, c.want)
		}
	}
}

// A NetBSD process reporting zero LWPs is forced to one thread, since the intake
// drops a process that reports zero threads.
func TestNetBSDZeroThreadsForcedToOne(t *testing.T) {
	blob := buildNetBSDProc(nbFields{pid: 5, stat: nbLSRUN, comm: "x", nlwps: 0})
	got := parseNetBSDProcs(blob)
	if len(got) != 1 || got[0].Threads != 1 {
		t.Fatalf("Threads = %v, want 1", got)
	}
}

// A trailing partial record is dropped, not decoded out of bounds.
func TestParseProcs2TruncatedTail(t *testing.T) {
	ob := append(buildOpenBSDProc(obFields{pid: 10, stat: obSRUN, comm: "a"}), make([]byte, obStride-1)...)
	if got := parseOpenBSDProcs(ob); len(got) != 1 || got[0].Pid != 10 {
		t.Errorf("openbsd: got %v, want pid [10] only", pids(got))
	}
	nb := append(buildNetBSDProc(nbFields{pid: 20, stat: nbLSRUN, comm: "b", nlwps: 1}), make([]byte, nbStride-1)...)
	if got := parseNetBSDProcs(nb); len(got) != 1 || got[0].Pid != 20 {
		t.Errorf("netbsd: got %v, want pid [20] only", pids(got))
	}
}

func TestOpenbsdState(t *testing.T) {
	cases := map[int8]ProcState{
		obSIDL: stateD, obSRUN: stateR, obSSLEEP: stateS, obSSTOP: stateT,
		obSZOMB: stateZ, obSDEAD: stateX, obSONPROC: stateR, 99: stateU,
	}
	for in, want := range cases {
		if got := openbsdState(in); got != want {
			t.Errorf("openbsdState(%d) = %d, want %d", in, got, want)
		}
	}
}

func TestNetbsdState(t *testing.T) {
	cases := map[int8]ProcState{
		nbLSIDL: stateD, nbLSRUN: stateR, nbLSSLEEP: stateS, nbLSSTOP: stateT,
		nbLSZOMB: stateZ, nbLSDEAD: stateX, nbLSONPROC: stateR, nbLSSUSPENDED: stateT, 99: stateU,
	}
	for in, want := range cases {
		if got := netbsdState(in); got != want {
			t.Errorf("netbsdState(%d) = %d, want %d", in, got, want)
		}
	}
}
