package host

// Synthetic parser tests for the OpenBSD/NetBSD disk-I/O decoders. The blobs are built at
// the offsets the parsers use, so a decode mismatch is a real offset regression. The
// vm_openbsd / vm_netbsd e2e confirm the offsets against a live kernel.

import (
	"encoding/binary"
	"math"
	"os"
	"testing"
	"time"
)

func buildOpenBSDDiskstats(name string, rxfer, wxfer, rbytes, wbytes uint64, busySec int64) []byte {
	s := make([]byte, obDsStride)
	copy(s[obDsName:obDsName+15], name)
	binary.LittleEndian.PutUint64(s[obDsRxfer:], rxfer)
	binary.LittleEndian.PutUint64(s[obDsWxfer:], wxfer)
	binary.LittleEndian.PutUint64(s[obDsRbytes:], rbytes)
	binary.LittleEndian.PutUint64(s[obDsWbytes:], wbytes)
	binary.LittleEndian.PutUint64(s[obDsTime:], uint64(busySec)) // timeval sec
	return s
}

func buildNetBSDIostat(name string, typ int32, rxfer, wxfer, rbytes, wbytes uint64, busySec uint32) []byte {
	s := make([]byte, nbIoStride)
	copy(s[nbIoName:nbIoName+15], name)
	binary.LittleEndian.PutUint32(s[nbIoType:], uint32(typ))
	binary.LittleEndian.PutUint32(s[nbIoTimeSec:], busySec)
	binary.LittleEndian.PutUint64(s[nbIoRxfer:], rxfer)
	binary.LittleEndian.PutUint64(s[nbIoWxfer:], wxfer)
	binary.LittleEndian.PutUint64(s[nbIoRbytes:], rbytes)
	binary.LittleEndian.PutUint64(s[nbIoWbytes:], wbytes)
	return s
}

func TestParseOpenBSDDiskstats(t *testing.T) {
	blob := append(buildOpenBSDDiskstats("sd0", 1000, 500, 4096000, 2048000, 3),
		buildOpenBSDDiskstats("", 9, 9, 9, 9, 9)...) // an empty-name record is skipped
	recs := parseOpenBSDDiskstats(blob)
	if len(recs) != 1 {
		t.Fatalf("recs = %d, want 1", len(recs))
	}
	r := recs[0]
	if r.name != "sd0" || r.readOps != 1000 || r.writeOps != 500 ||
		r.readBytes != 4096000 || r.writeBytes != 2048000 || r.busyS != 3 {
		t.Errorf("rec = %+v", r)
	}
	// Rate math over dt=10 from a zero baseline, including util from the busy time.
	s := bsdIOSeries(r.name, r, devstatRec{name: "sd0"}, 10, time.Unix(1, 0))
	want := map[string]float64{
		"system.io.r_s": 100, "system.io.w_s": 50,
		"system.io.rkb_s": 400, "system.io.wkb_s": 200, "system.io.util": 30,
	}
	for n, w := range want {
		if g := devstatVal(s, n); math.Abs(g-w) > 1e-9 {
			t.Errorf("%s = %v, want %v", n, g, w)
		}
	}
	if len(s) != 5 {
		t.Errorf("got %d series, want 5 (r_s/w_s/rkb_s/wkb_s/util)", len(s))
	}
}

func TestParseNetBSDIostats(t *testing.T) {
	blob := append(buildNetBSDIostat("wd0", 0, 1000, 500, 4096000, 2048000, 3),
		buildNetBSDIostat("nfs0", 2, 9, 9, 9, 9, 9)...) // a non-disk (type != 0) is filtered
	recs := parseNetBSDIostats(blob)
	if len(recs) != 1 {
		t.Fatalf("recs = %d, want 1 (non-disk filtered)", len(recs))
	}
	r := recs[0]
	if r.name != "wd0" || r.readOps != 1000 || r.writeOps != 500 ||
		r.readBytes != 4096000 || r.writeBytes != 2048000 || r.busyS != 3 {
		t.Errorf("rec = %+v", r)
	}
}

func TestBSDIOCounterReset(t *testing.T) {
	cur := devstatRec{name: "sd0", readOps: 5}
	prev := devstatRec{name: "sd0", readOps: 100} // cur < prev: a reset
	if s := bsdIOSeries("sd0", cur, prev, 10, time.Unix(1, 0)); s != nil {
		t.Errorf("counter reset should drop the device, got %d series", len(s))
	}
}

// TestParseOpenBSDDiskstatsGolden decodes a real HW_DISKSTATS blob captured from OpenBSD
// 7.9 amd64 by the vm_openbsd e2e (sd0 is the active root disk).
func TestParseOpenBSDDiskstatsGolden(t *testing.T) {
	b, err := os.ReadFile("testdata/openbsd_diskstats.bin")
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]devstatRec{}
	for _, r := range parseOpenBSDDiskstats(b) {
		byName[r.name] = r
	}
	sd0, ok := byName["sd0"]
	if !ok {
		t.Fatalf("no sd0 in %v", byName)
	}
	if sd0.readOps != 27166 || sd0.writeOps != 14505 || sd0.readBytes != 574460416 {
		t.Errorf("sd0 = %+v, want readOps 27166 writeOps 14505 readBytes 574460416", sd0)
	}
	for _, n := range []string{"cd0", "fd0"} {
		if _, ok := byName[n]; !ok {
			t.Errorf("expected %q in the captured table", n)
		}
	}
}

// TestParseNetBSDIostatsGolden decodes a real HW_IOSTATS blob captured from NetBSD 10.1
// amd64 by the vm_netbsd e2e (ld0 is the active root disk).
func TestParseNetBSDIostatsGolden(t *testing.T) {
	b, err := os.ReadFile("testdata/netbsd_iostats.bin")
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]devstatRec{}
	for _, r := range parseNetBSDIostats(b) {
		byName[r.name] = r
	}
	ld0, ok := byName["ld0"]
	if !ok {
		t.Fatalf("no ld0 in %v", byName)
	}
	if ld0.readOps != 15094 || ld0.writeOps != 2517 || ld0.readBytes != 178268160 {
		t.Errorf("ld0 = %+v, want readOps 15094 writeOps 2517 readBytes 178268160", ld0)
	}
}
