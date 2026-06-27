package process

import (
	"bytes"
	"math"
	"reflect"
	"testing"
)

func TestFrameHeader(t *testing.T) {
	got := frame(typeCollectorProc, []byte{0xAA, 0xBB})
	want := []byte{3, 0, 12, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0xAA, 0xBB}
	if !bytes.Equal(got, want) {
		t.Errorf("frame = % x, want % x", got, want)
	}
	body, typ, ok := unframe(got)
	if !ok || typ != typeCollectorProc || !bytes.Equal(body, []byte{0xAA, 0xBB}) {
		t.Errorf("unframe = %x, %d, %v", body, typ, ok)
	}
	if _, _, ok := unframe([]byte{1, 2, 3}); ok {
		t.Error("unframe accepted a short header")
	}
}

func TestEncodeCommandGolden(t *testing.T) {
	// Args=["ls","-l"], Exe="/bin/ls". Cwd/Ppid/Comm zero -> omitted.
	got := encodeCommand(&Proc{Args: []string{"ls", "-l"}, Exe: "/bin/ls"})
	want := []byte{
		0x0A, 0x02, 'l', 's', // field 1 (Args) "ls"
		0x0A, 0x02, '-', 'l', // field 1 (Args) "-l"
		0x42, 0x07, '/', 'b', 'i', 'n', '/', 'l', 's', // field 8 (Exe)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("encodeCommand = % x, want % x", got, want)
	}
}

func TestEncodeCPUFixed32Golden(t *testing.T) {
	// Only TotalPct set: tag = field 2, wire 5 = 0x15. 1.5f = 0x3FC00000 LE.
	got := encodeCPU(&Proc{TotalPct: 1.5})
	want := []byte{0x15, 0x00, 0x00, 0xC0, 0x3F}
	if !bytes.Equal(got, want) {
		t.Errorf("encodeCPU = % x, want % x", got, want)
	}
}

func TestCollectorProcRoundTrip(t *testing.T) {
	in := &Proc{
		Pid: 1234, Ppid: 1, NsPid: 1234,
		Name: "agent", Exe: "/bin/agent", Cwd: "/", Args: []string{"/bin/agent", "-debug"},
		User: "root", Uid: 0, Gid: 0,
		State: stateR, CreateTime: 1_700_000_000_000, Threads: 7, OpenFd: 5,
		Rss: 4096 * 100, Vms: 4096 * 500, Swap: 0, Shared: 4096 * 10,
		UserTime: 12, SystemTime: 3, TotalPct: 4.5, UserPct: 3.5, SystemPct: 1.0,
		VoluntaryCtxSwitches: 42, InvoluntaryCtxSwitches: 9,
	}
	framed := frame(typeCollectorProc, encodeCollectorProc("host-a", sysInfo{os: "linux"}, []*Proc{in}, 7, 1))

	body, typ, ok := unframe(framed)
	if !ok || typ != typeCollectorProc {
		t.Fatalf("unframe failed: typ=%d ok=%v", typ, ok)
	}
	host, procs := decodeCollectorProc(t, body)
	if host != "host-a" {
		t.Errorf("hostname = %q, want host-a", host)
	}
	if len(procs) != 1 {
		t.Fatalf("got %d processes, want 1", len(procs))
	}
	got := procs[0]
	// Compare the fields that survive the wire (Pct/time floats become the same).
	want := *in
	if !reflect.DeepEqual(got, want) {
		t.Errorf("round-trip mismatch:\n got  %+v\n want %+v", got, want)
	}
}

func TestEncodeSystemInfoTotalMemory(t *testing.T) {
	// totalMemory must reach SystemInfo field 5 (the Live Processes backend uses it
	// to compute per-process MEM%). A zero would be omitted, leaving it unset.
	const want = 16 << 30
	body := encodeSystemInfo(sysInfo{os: "linux", platform: "linux", totalMemory: want})
	r := &pread{b: body}
	var total uint64
	for {
		f, _, _, v, ok := r.next()
		if !ok {
			break
		}
		if f == 5 {
			total = v
		}
	}
	if total != want {
		t.Errorf("SystemInfo.totalMemory = %d, want %d", total, uint64(want))
	}
}

func TestReadResCollector(t *testing.T) {
	// Build a ResCollector: Status (field 3) -> {ActiveClients=3, Interval=2}.
	var status pbuf
	status.uint(1, 3)
	status.uint(2, 2)
	var res pbuf
	res.msg(3, status.b)
	framed := frame(typeResCollector, res.b)

	active, interval, ok := readResCollector(framed)
	if !ok || active != 3 || interval != 2 {
		t.Errorf("readResCollector = %d, %d, %v; want 3, 2, true", active, interval, ok)
	}

	// A CollectorProc-typed message is not a response we act on.
	if _, _, ok := readResCollector(frame(typeCollectorProc, res.b)); ok {
		t.Error("readResCollector accepted a non-ResCollector type")
	}
}

// A hostile response body must never panic the reader. Field 3 with wire type 2
// here claims a length near 2^63, far past the buffer.
func TestReadResCollectorHostileLength(t *testing.T) {
	body := []byte{0x1a, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0x7f}
	if active, interval, ok := readResCollector(frame(typeResCollector, body)); !ok || active != 0 || interval != 0 {
		t.Errorf("hostile length = %d, %d, %v; want 0, 0, true (the malformed field reads as absent)", active, interval, ok)
	}
}

func TestVarintRoundTrip(t *testing.T) {
	for _, v := range []uint64{0, 1, 127, 128, 300, 1 << 20, math.MaxUint32, math.MaxUint64} {
		var w pbuf
		w.uvarint(v)
		r := &pread{b: w.b}
		got, ok := r.varint()
		if !ok || got != v || r.i != len(w.b) {
			t.Errorf("varint %d round-trip = %d, ok=%v, consumed=%d/%d", v, got, ok, r.i, len(w.b))
		}
	}
}

// test-only decoders (exercise the same field numbers the encoder writes)

func decodeCollectorProc(t *testing.T, b []byte) (host string, procs []Proc) {
	t.Helper()
	r := &pread{b: b}
	for {
		f, _, data, _, ok := r.next()
		if !ok {
			return host, procs
		}
		switch f {
		case 2:
			host = string(data)
		case 3:
			procs = append(procs, decodeProcess(data))
		}
	}
}

func decodeProcess(b []byte) Proc {
	var p Proc
	r := &pread{b: b}
	for {
		f, _, data, val, ok := r.next()
		if !ok {
			return p
		}
		switch f {
		case 2:
			p.Pid = int32(val)
		case 4:
			cr := &pread{b: data}
			for {
				cf, _, cd, cv, cok := cr.next()
				if !cok {
					break
				}
				switch cf {
				case 1:
					p.Args = append(p.Args, string(cd))
				case 3:
					p.Cwd = string(cd)
				case 6:
					p.Ppid = int32(cv)
				case 8:
					p.Exe = string(cd)
				case 9:
					p.Name = string(cd)
				}
			}
		case 5:
			ur := &pread{b: data}
			for {
				uf, _, ud, uv, uok := ur.next()
				if !uok {
					break
				}
				switch uf {
				case 1:
					p.User = string(ud)
				case 2:
					p.Uid = int32(uv)
				case 3:
					p.Gid = int32(uv)
				}
			}
		case 7:
			mr := &pread{b: data}
			for {
				mf, _, _, mv, mok := mr.next()
				if !mok {
					break
				}
				switch mf {
				case 1:
					p.Rss = mv
				case 2:
					p.Vms = mv
				case 3:
					p.Swap = mv
				case 4:
					p.Shared = mv
				}
			}
		case 8:
			cr := &pread{b: data}
			for {
				cf, _, cd, cv, cok := cr.next()
				if !cok {
					break
				}
				switch cf {
				case 2:
					p.TotalPct = f32(cd)
				case 3:
					p.UserPct = f32(cd)
				case 4:
					p.SystemPct = f32(cd)
				case 5:
					p.Threads = int32(cv)
				case 8:
					p.UserTime = float64(cv)
				case 9:
					p.SystemTime = float64(cv)
				}
			}
		case 9:
			p.CreateTime = int64(val)
		case 11:
			p.OpenFd = int32(val)
		case 12:
			p.State = ProcState(val)
		case 16:
			p.VoluntaryCtxSwitches = val
		case 17:
			p.InvoluntaryCtxSwitches = val
		case 20:
			p.NsPid = int32(val)
		}
	}
}

func f32(b []byte) float32 {
	if len(b) != 4 {
		return 0
	}
	return math.Float32frombits(uint32(b[0]) | uint32(b[1])<<8 | uint32(b[2])<<16 | uint32(b[3])<<24)
}
