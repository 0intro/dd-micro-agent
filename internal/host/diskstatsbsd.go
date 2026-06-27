package host

// Disk I/O decoders for OpenBSD (HW_DISKSTATS, struct diskstats, sys/sys/disk.h) and
// NetBSD (HW_IOSTATS, struct io_sysctl, sys/sys/iostat.h). Both are arrays of fixed-size
// records with named scalar counters and, unlike DragonFly's devstat, a usable busy-time
// field, so both emit util as well as the throughput rates. Neither carries per-operation
// durations, so neither has the awaits FreeBSD adds. Offsets are pinned for the amd64 ABI
// and verified against a live kernel by the vm_openbsd / vm_netbsd e2e. Neutral (no build
// tag) so the parsers unit-test on the dev host, io_openbsd.go / io_netbsd.go make the
// numeric-MIB sysctl call.

import (
	"encoding/binary"
	"time"

	"github.com/0intro/dd-micro-agent/internal/metrics"
)

// OpenBSD struct diskstats offsets (amd64, DS_DISKNAMELEN 16). ds_time is a struct timeval
// of total busy time.
const (
	obDsStride = 112
	obDsName   = 0  // char ds_name[16]
	obDsRxfer  = 24 // u_int64_t ds_rxfer  (read transfers)
	obDsWxfer  = 32 // u_int64_t ds_wxfer  (write transfers)
	obDsRbytes = 48 // u_int64_t ds_rbytes
	obDsWbytes = 56 // u_int64_t ds_wbytes
	obDsTime   = 96 // struct timeval ds_time (busy)
)

func parseOpenBSDDiskstats(b []byte) []devstatRec {
	var out []devstatRec
	for off := 0; off+obDsStride <= len(b); off += obDsStride {
		s := b[off : off+obDsStride]
		name := trimNul(s[obDsName : obDsName+16])
		if name == "" {
			continue
		}
		out = append(out, devstatRec{
			name:       name,
			readOps:    u64f(s, obDsRxfer),
			writeOps:   u64f(s, obDsWxfer),
			readBytes:  u64f(s, obDsRbytes),
			writeBytes: u64f(s, obDsWbytes),
			busyS:      timevalSec(s, obDsTime), // {int64 sec; int64 usec}
		})
	}
	return out
}

// NetBSD struct io_sysctl offsets (amd64, IOSTATNAMELEN 16). It keeps busy time as a
// split u_int32 sec/usec pair, and a type field (0 = disk).
const (
	nbIoStride  = 128
	nbIoName    = 0  // char name[16]
	nbIoType    = 20 // int32_t type (IOSTAT_DISK == 0)
	nbIoTimeSec = 64 // u_int32_t time_sec (busy), time_usec follows
	nbIoRxfer   = 72 // u_int64_t rxfer
	nbIoRbytes  = 80 // u_int64_t rbytes
	nbIoWxfer   = 88 // u_int64_t wxfer
	nbIoWbytes  = 96 // u_int64_t wbytes
)

func parseNetBSDIostats(b []byte) []devstatRec {
	var out []devstatRec
	for off := 0; off+nbIoStride <= len(b); off += nbIoStride {
		s := b[off : off+nbIoStride]
		if int32(binary.LittleEndian.Uint32(s[nbIoType:])) != 0 { // disks only, not tape/nfs
			continue
		}
		name := trimNul(s[nbIoName : nbIoName+16])
		if name == "" {
			continue
		}
		sec := binary.LittleEndian.Uint32(s[nbIoTimeSec:])
		usec := binary.LittleEndian.Uint32(s[nbIoTimeSec+4:])
		out = append(out, devstatRec{
			name:       name,
			readOps:    u64f(s, nbIoRxfer),
			writeOps:   u64f(s, nbIoWxfer),
			readBytes:  u64f(s, nbIoRbytes),
			writeBytes: u64f(s, nbIoWbytes),
			busyS:      float64(sec) + float64(usec)/1e6,
		})
	}
	return out
}

// timevalSec decodes a struct timeval { time_t sec; suseconds_t usec } (amd64: two 8-byte
// little-endian integers) at offset o to seconds.
func timevalSec(s []byte, o int) float64 {
	sec := int64(binary.LittleEndian.Uint64(s[o:]))
	usec := int64(binary.LittleEndian.Uint64(s[o+8:]))
	return float64(sec) + float64(usec)/1e6
}

// bsdIOSeries computes the iostat-style rates for one device over [p, c] / dt. OpenBSD and
// NetBSD both supply a busy time, so this emits util. Neither has per-operation durations,
// so neither has the awaits FreeBSD adds.
func bsdIOSeries(name string, c, p devstatRec, dt float64, now time.Time) []metrics.Serie {
	dRead, dWrite := c.readOps-p.readOps, c.writeOps-p.writeOps
	if dRead < 0 || dWrite < 0 {
		return nil // counter reset or device replaced
	}
	dev := "device:" + name
	util := nonneg(c.busyS-p.busyS) / dt * 100 // busy-time fraction → %
	if util > 100 {
		util = 100
	}
	return []metrics.Serie{
		gauge("system.io.r_s", now, dRead/dt, dev),
		gauge("system.io.w_s", now, dWrite/dt, dev),
		gauge("system.io.rkb_s", now, nonneg(c.readBytes-p.readBytes)/1024/dt, dev),
		gauge("system.io.wkb_s", now, nonneg(c.writeBytes-p.writeBytes)/1024/dt, dev),
		gauge("system.io.util", now, util, dev),
	}
}
