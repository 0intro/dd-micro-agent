package process

// FreeBSD process decoding. parseKinfoProcs unpacks a `kern.proc.all` sysctl blob,
// an array of variable-but-self-sized `struct kinfo_proc` (sys/sys/user.h), into
// Procs. Each record starts with an int ki_structsize giving its own length, which is
// the iteration stride. The field offsets are pinned for the amd64 ABI and stable
// across FreeBSD 12 to 15 (where ki_structsize is 1088). The offsets are verified against
// a real kern.proc.all blob + ps by the vm_freebsd e2e. This file carries no build tag
// so the parser unit-tests on the dev host. collect_freebsd.go (freebsd-only) makes
// the sysctl call.

import (
	"bytes"
	"encoding/binary"
)

// pageSize is the amd64 base page size. ki_rssize is reported in pages. Pinned to the
// amd64 target (like the offsets below) rather than read from the running host.
const pageSize = 4096

// Field offsets within one `struct kinfo_proc` (FreeBSD amd64). kpStructsize holds the
// record's self-describing length (the stride). kpMinSize is one past the highest byte
// we read (ru_nivcsw ends at 752), so a record must be at least that large to decode.
const (
	kpStructsize = 0   // int           ki_structsize (the stride)
	kpPid        = 72  // pid_t         ki_pid
	kpPpid       = 76  // pid_t         ki_ppid
	kpRuid       = 172 // uid_t         ki_ruid
	kpRgid       = 180 // gid_t         ki_rgid
	kpSize       = 256 // vm_size_t     ki_size   (bytes) -> Vms
	kpRssize     = 264 // segsz_t       ki_rssize (pages) -> Rss
	kpStart      = 336 // struct timeval ki_start         -> CreateTime
	kpStat       = 388 // char          ki_stat           -> State
	kpComm       = 447 // char          ki_comm[COMMLEN+1]
	kpCommLen    = 20  // COMMLEN+1
	kpNumthreads = 596 // int           ki_numthreads
	kpUtime      = 608 // struct timeval ki_rusage.ru_utime
	kpStime      = 624 // struct timeval ki_rusage.ru_stime
	kpNvcsw      = 736 // long          ki_rusage.ru_nvcsw
	kpNivcsw     = 744 // long          ki_rusage.ru_nivcsw
	kpMinSize    = 752 // one past ru_nivcsw, the smallest record we accept
)

// FreeBSD process states: the ki_stat integer enum (sys/proc.h).
const (
	sIDL   = 1 // being created
	sRUN   = 2 // runnable
	sSLEEP = 3 // sleeping on an address
	sSTOP  = 4 // process debugging or suspension
	sZOMB  = 5 // awaiting collection by parent
	sWAIT  = 6 // waiting for an interrupt
	sLOCK  = 7 // blocked on a lock
)

// parseKinfoProcs decodes the kern.proc.all blob (one struct kinfo_proc per process).
// Args/Exe/User are not in kinfo_proc and are filled by collect_freebsd.go.
func parseKinfoProcs(b []byte) []Proc {
	var out []Proc
	for off := 0; off+4 <= len(b); {
		ss := int(int32(binary.LittleEndian.Uint32(b[off:])))
		if ss < kpMinSize || off+ss > len(b) {
			break // zero/garbage/negative size, or a truncated trailing record
		}
		r := b[off : off+ss]
		off += ss

		p := Proc{
			Pid:                    int32(binary.LittleEndian.Uint32(r[kpPid:])),
			Ppid:                   int32(binary.LittleEndian.Uint32(r[kpPpid:])),
			Uid:                    int32(binary.LittleEndian.Uint32(r[kpRuid:])),
			Gid:                    int32(binary.LittleEndian.Uint32(r[kpRgid:])),
			Name:                   cstr(r[kpComm : kpComm+kpCommLen]),
			State:                  freebsdState(r[kpStat]),
			Threads:                int32(binary.LittleEndian.Uint32(r[kpNumthreads:])),
			Vms:                    binary.LittleEndian.Uint64(r[kpSize:]),
			CreateTime:             tvMillis(r, kpStart),
			UserTime:               tvSeconds(r, kpUtime),
			SystemTime:             tvSeconds(r, kpStime),
			VoluntaryCtxSwitches:   binary.LittleEndian.Uint64(r[kpNvcsw:]),
			InvoluntaryCtxSwitches: binary.LittleEndian.Uint64(r[kpNivcsw:]),
		}
		p.NsPid = p.Pid
		// ki_rssize is signed (segsz_t). Clamp a stray negative so it can't become a
		// huge unsigned RSS.
		if pages := int64(binary.LittleEndian.Uint64(r[kpRssize:])); pages > 0 {
			p.Rss = uint64(pages) * pageSize
		}
		out = append(out, p)
	}
	return out
}

// freebsdState maps a ki_stat enum value to the wire enum.
func freebsdState(st byte) ProcState {
	switch st {
	case sRUN:
		return stateR
	case sSLEEP, sWAIT:
		return stateS
	case sSTOP:
		return stateT
	case sZOMB:
		return stateZ
	case sLOCK, sIDL:
		return stateD
	default:
		return stateU
	}
}

// parseArgv splits a NUL-separated argument vector (FreeBSD kern.proc.args) into
// arguments, dropping empties (a kernel process reports an empty vector).
func parseArgv(b []byte) []string {
	var out []string
	for _, a := range bytes.Split(b, []byte{0}) {
		if len(a) > 0 {
			out = append(out, string(a))
		}
	}
	return out
}

// cstr returns the NUL-terminated C string at the start of b.
func cstr(b []byte) string {
	if i := bytes.IndexByte(b, 0); i >= 0 {
		return string(b[:i])
	}
	return string(b)
}

// tvSeconds decodes a `struct timeval { time_t sec; suseconds_t usec }` (amd64: two
// 8-byte little-endian signed integers) at offset o to seconds.
func tvSeconds(b []byte, o int) float64 {
	sec := int64(binary.LittleEndian.Uint64(b[o:]))
	usec := int64(binary.LittleEndian.Uint64(b[o+8:]))
	return float64(sec) + float64(usec)/1e6
}

// tvMillis decodes the same timeval to epoch milliseconds (ki_start is absolute).
func tvMillis(b []byte, o int) int64 {
	sec := int64(binary.LittleEndian.Uint64(b[o:]))
	usec := int64(binary.LittleEndian.Uint64(b[o+8:]))
	return sec*1000 + usec/1000
}
