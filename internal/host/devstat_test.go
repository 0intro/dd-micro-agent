package host

// Self-contained (no Linux-only test helpers) so it runs on the dev host. The blob is
// synthesized at the offsets parseDevstat uses. Those offsets were verified against a
// real FreeBSD 15.1 kern.devstat.all sample by the vm_freebsd e2e.

import (
	"encoding/binary"
	"math"
	"testing"
	"time"

	"github.com/0intro/dd-micro-agent/internal/metrics"
)

func putBintime(s []byte, off int, sec int64, frac uint64) {
	binary.LittleEndian.PutUint64(s[off:], uint64(sec))
	binary.LittleEndian.PutUint64(s[off+8:], frac)
}

func TestParseDevstat(t *testing.T) {
	blob := make([]byte, devstatGen+2*devstatStride)

	// device 0: "ada"+unit 0 = "ada0", active.
	d0 := blob[devstatGen : devstatGen+devstatStride]
	copy(d0[dsName:], "ada")
	binary.LittleEndian.PutUint32(d0[dsUnit:], 0)
	binary.LittleEndian.PutUint64(d0[dsOps+8*transRead:], 1000)
	binary.LittleEndian.PutUint64(d0[dsOps+8*transWrite:], 500)
	binary.LittleEndian.PutUint64(d0[dsBytes+8*transRead:], 4096000)
	binary.LittleEndian.PutUint64(d0[dsBytes+8*transWrite:], 2048000)
	putBintime(d0, dsDur+16*transRead, 2, 1<<63) // 2.5s (tests the frac term)
	putBintime(d0, dsDur+16*transWrite, 1, 0)    // 1.0s
	putBintime(d0, dsBusy, 3, 0)                 // 3.0s

	// device 1: "cd"+unit 1 = "cd1", idle.
	d1 := blob[devstatGen+devstatStride:]
	copy(d1[dsName:], "cd")
	binary.LittleEndian.PutUint32(d1[dsUnit:], 1)

	recs := parseDevstat(blob)
	if len(recs) != 2 {
		t.Fatalf("recs = %d, want 2", len(recs))
	}
	r := recs[0]
	if r.name != "ada0" || r.readOps != 1000 || r.writeOps != 500 ||
		r.readBytes != 4096000 || r.writeBytes != 2048000 {
		t.Errorf("dev0 = %+v", r)
	}
	if r.readDurS != 2.5 || r.writeDurS != 1.0 || r.busyS != 3.0 {
		t.Errorf("dev0 durations = %v/%v/%v, want 2.5/1/3", r.readDurS, r.writeDurS, r.busyS)
	}
	if recs[1].name != "cd1" {
		t.Errorf("dev1 name = %q, want cd1", recs[1].name)
	}

	// Rate math over dt=10 from a zero baseline.
	series := devstatSeries(r.name, r, devstatRec{name: "ada0"}, 10, time.Unix(1000, 0))
	want := map[string]float64{
		"system.io.r_s": 100, "system.io.w_s": 50,
		"system.io.rkb_s": 400, "system.io.wkb_s": 200,
		"system.io.util": 30, "system.io.await": 3500.0 / 1500,
		"system.io.r_await": 2.5, "system.io.w_await": 2.0,
	}
	for name, w := range want {
		if g := devstatVal(series, name); math.Abs(g-w) > 1e-9 {
			t.Errorf("%s = %v, want %v", name, g, w)
		}
	}
}

func devstatVal(series []metrics.Serie, name string) float64 {
	for _, s := range series {
		if s.Name == name && len(s.Points) > 0 {
			return s.Points[0].Value
		}
	}
	return math.NaN()
}
