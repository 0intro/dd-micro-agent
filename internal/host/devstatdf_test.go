package host

// Self-contained parser tests for the DragonFly devstat decoder. The synthetic blob is
// built at the offsets parseDevstatDF uses, and the golden test decodes a real
// kern.devstat.all sample captured from DragonFly 6.4.2 by the vm_dragonfly e2e.

import (
	"encoding/binary"
	"math"
	"os"
	"testing"
	"time"
)

func buildDevstatDF(name string, unit int32, bytesRead, bytesWrite, numReads, numWrites uint64) []byte {
	b := make([]byte, dfDevstatGen+dfDevstatStride)
	s := b[dfDevstatGen:]
	copy(s[dfDsName:dfDsName+15], name)
	binary.LittleEndian.PutUint32(s[dfDsUnit:], uint32(unit))
	binary.LittleEndian.PutUint64(s[dfDsBytesRead:], bytesRead)
	binary.LittleEndian.PutUint64(s[dfDsBytesWrite:], bytesWrite)
	binary.LittleEndian.PutUint64(s[dfDsNumReads:], numReads)
	binary.LittleEndian.PutUint64(s[dfDsNumWrites:], numWrites)
	return b
}

func TestParseDevstatDF(t *testing.T) {
	blob := buildDevstatDF("vbd", 0, 4096000, 2048000, 1000, 500)
	recs := parseDevstatDF(blob)
	if len(recs) != 1 {
		t.Fatalf("recs = %d, want 1", len(recs))
	}
	r := recs[0]
	if r.name != "vbd0" || r.readOps != 1000 || r.writeOps != 500 ||
		r.readBytes != 4096000 || r.writeBytes != 2048000 {
		t.Errorf("dev = %+v", r)
	}

	// Rate math over dt=10 from a zero baseline: throughput only, no util or awaits.
	series := devstatDFSeries(r.name, r, devstatRec{name: "vbd0"}, 10, time.Unix(1000, 0))
	want := map[string]float64{
		"system.io.r_s": 100, "system.io.w_s": 50,
		"system.io.rkb_s": 400, "system.io.wkb_s": 200,
	}
	for name, w := range want {
		if g := devstatVal(series, name); math.Abs(g-w) > 1e-9 {
			t.Errorf("%s = %v, want %v", name, g, w)
		}
	}
	for _, absent := range []string{"system.io.util", "system.io.await", "system.io.r_await", "system.io.w_await"} {
		if !math.IsNaN(devstatVal(series, absent)) {
			t.Errorf("%s should not be emitted on DragonFly", absent)
		}
	}
	if len(series) != 4 {
		t.Errorf("got %d series, want 4 (r_s/w_s/rkb_s/wkb_s)", len(series))
	}
	if tags := series[0].Tags; len(tags) != 1 || tags[0] != "device:vbd0" {
		t.Errorf("tags = %v, want [device:vbd0]", tags)
	}
}

func TestDevstatDFCounterReset(t *testing.T) {
	cur := devstatRec{name: "vbd0", readOps: 5}
	prev := devstatRec{name: "vbd0", readOps: 100} // a reset: cur < prev
	if s := devstatDFSeries("vbd0", cur, prev, 10, time.Unix(1, 0)); s != nil {
		t.Errorf("counter reset should drop the device, got %d series", len(s))
	}
}

// TestParseDevstatDFGolden decodes a real kern.devstat.all blob captured from DragonFly
// 6.4.2 amd64, the ground truth for the pinned offsets. vbd0 is the virtio root disk.
func TestParseDevstatDFGolden(t *testing.T) {
	b, err := os.ReadFile("testdata/dragonfly_devstat.bin")
	if err != nil {
		t.Fatal(err)
	}
	recs := parseDevstatDF(b)
	if len(recs) != (len(b)-dfDevstatGen)/dfDevstatStride || len(recs) == 0 {
		t.Fatalf("got %d recs from %d bytes", len(recs), len(b))
	}
	byName := map[string]devstatRec{}
	for _, r := range recs {
		byName[r.name] = r
	}
	vbd0, ok := byName["vbd0"]
	if !ok {
		t.Fatalf("no vbd0 in %v", byName)
	}
	if vbd0.readOps != 1144 || vbd0.writeOps != 65 || vbd0.readBytes != 43738112 || vbd0.writeBytes != 688128 {
		t.Errorf("vbd0 = %+v, want readOps 1144 writeOps 65 readBytes 43738112 writeBytes 688128", vbd0)
	}
}
