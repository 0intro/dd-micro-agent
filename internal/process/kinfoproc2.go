package process

// OpenBSD and NetBSD process decoding. Each kernel returns its process table as a
// packed array of a fixed-size struct: OpenBSD's KERN_PROC gives struct kinfo_proc
// (sys/sysctl.h), NetBSD's KERN_PROC2 gives struct kinfo_proc2. The two layouts
// differ (NetBSD has an extra leading pointer and a thread count, OpenBSD carries
// the virtual size in bytes while NetBSD carries it in pages, and the process-state
// enums differ), so each gets its own parser and offset block. The offsets are
// pinned for the amd64 ABI: OpenBSD's were read with offsetof on a 7.9 VM, NetBSD's
// computed from the netbsd-10 struct (fixed-width fields, so the amd64 layout is
// ABI-determined). Both are verified against a live blob plus ps by the vm_openbsd
// and vm_netbsd e2e tests. This file carries no build tag so the parsers unit-test
// on the dev host. The per-OS collect files make the sysctl call.
//
// These structs store the start, user, and system times as separate sec and usec
// scalar fields (not an embedded timeval), so the timeval helpers in kinfoproc.go do
// not apply here. cstr, parseArgv, and pageSize are shared from kinfoproc.go.

import "encoding/binary"

func le32(b []byte, o int) uint32 { return binary.LittleEndian.Uint32(b[o:]) }
func le64(b []byte, o int) uint64 { return binary.LittleEndian.Uint64(b[o:]) }

// usecMillis converts a sec+usec pair to epoch milliseconds (for an absolute start
// time). usecSeconds converts it to seconds (for cumulative CPU time).
func usecMillis(sec, usec int64) int64    { return sec*1000 + usec/1000 }
func usecSeconds(sec, usec int64) float64 { return float64(sec) + float64(usec)/1e6 }

// OpenBSD struct kinfo_proc, amd64. obStride is sizeof (the record stride).
const (
	obStride    = 648
	obComLen    = 24 // KI_MAXCOMLEN
	obPid       = 108
	obPpid      = 112
	obRuid      = 132
	obRgid      = 140
	obStat      = 304 // int8 p_stat
	obComm      = 312
	obRssize    = 384 // int32, pages
	obTsize     = 388 // int32, pages (text)
	obDsize     = 392 // int32, pages (data)
	obSsize     = 396 // int32, pages (stack)
	obStartSec  = 408 // uint64
	obStartUsec = 416 // uint32
	obUtimeSec  = 420 // uint32
	obUtimeUsec = 424
	obStimeSec  = 428
	obStimeUsec = 432
	obNvcsw     = 536 // uint64 p_uru_nvcsw
	obNivcsw    = 544
)

// OpenBSD p_stat values (sys/proc.h).
const (
	obSIDL    = 1
	obSRUN    = 2
	obSSLEEP  = 3
	obSSTOP   = 4
	obSZOMB   = 5
	obSDEAD   = 6
	obSONPROC = 7
)

// parseOpenBSDProcs decodes a KERN_PROC blob (struct kinfo_proc per process). Args,
// Exe, and User are not in the struct and are filled by collect_openbsd.go. OpenBSD
// reports per-process entries with no thread count, so Threads is 1 (a real value,
// and the intake drops any process that reports zero threads).
func parseOpenBSDProcs(b []byte) []Proc {
	var out []Proc
	for off := 0; off+obStride <= len(b); off += obStride {
		r := b[off : off+obStride]
		p := Proc{
			Pid:                    int32(le32(r, obPid)),
			Ppid:                   int32(le32(r, obPpid)),
			Uid:                    int32(le32(r, obRuid)),
			Gid:                    int32(le32(r, obRgid)),
			Name:                   cstr(r[obComm : obComm+obComLen]),
			State:                  openbsdState(int8(r[obStat])),
			Threads:                1,
			CreateTime:             usecMillis(int64(le64(r, obStartSec)), int64(le32(r, obStartUsec))),
			UserTime:               usecSeconds(int64(le32(r, obUtimeSec)), int64(le32(r, obUtimeUsec))),
			SystemTime:             usecSeconds(int64(le32(r, obStimeSec)), int64(le32(r, obStimeUsec))),
			VoluntaryCtxSwitches:   le64(r, obNvcsw),
			InvoluntaryCtxSwitches: le64(r, obNivcsw),
		}
		p.NsPid = p.Pid
		if pages := int64(int32(le32(r, obRssize))); pages > 0 {
			p.Rss = uint64(pages) * pageSize
		}
		// Vms is text+data+stack, ps VSZ. Those are the segment counts the
		// kernel's FILL_KPROC fills for this sysctl (p_vm_map_size stays zero).
		if vpages := int64(int32(le32(r, obTsize))) + int64(int32(le32(r, obDsize))) + int64(int32(le32(r, obSsize))); vpages > 0 {
			p.Vms = uint64(vpages) * pageSize
		}
		out = append(out, p)
	}
	return out
}

// parseOpenBSDArgv decodes OpenBSD's KERN_PROC_ARGV buffer: an argv array of
// pointers, NULL-terminated, followed by the strings themselves (sysctl(2)).
// The pointer values address the caller's own buffer and carry nothing useful,
// so the NUL-separated strings after the NULL entry are the argv words. The
// 8-byte pointer width is the same amd64 pin as the struct offsets above.
func parseOpenBSDArgv(b []byte) []string {
	for off := 0; off+8 <= len(b); off += 8 {
		if le64(b, off) == 0 {
			return parseArgv(b[off+8:])
		}
	}
	return nil
}

func openbsdState(st int8) ProcState {
	switch st {
	case obSRUN, obSONPROC:
		return stateR
	case obSSLEEP:
		return stateS
	case obSSTOP:
		return stateT
	case obSZOMB:
		return stateZ
	case obSDEAD:
		return stateX
	case obSIDL:
		return stateD
	default:
		return stateU
	}
}

// NetBSD struct kinfo_proc2, amd64. nbStride is sizeof (the record stride).
const (
	nbStride    = 680
	nbComLen    = 24 // KI_MAXCOMLEN
	nbPid       = 116
	nbPpid      = 120
	nbRuid      = 140
	nbRgid      = 148
	nbStat      = 360 // int8 p_stat (the LWP state)
	nbComm      = 368
	nbRssize    = 432 // int32, pages
	nbStartSec  = 456 // uint32
	nbStartUsec = 460
	nbUtimeSec  = 464
	nbUtimeUsec = 468
	nbStimeSec  = 472
	nbStimeUsec = 476
	nbNvcsw     = 576 // uint64 p_uru_nvcsw
	nbNivcsw    = 584
	nbNlwps     = 616 // uint64, thread count
	nbVsize     = 664 // int64 p_vm_vsize, pages (Vms = pages * pageSize)
)

// NetBSD LWP states (sys/lwp.h), since p_stat carries the LWP status.
const (
	nbLSIDL       = 1
	nbLSRUN       = 2
	nbLSSLEEP     = 3
	nbLSSTOP      = 4
	nbLSZOMB      = 5
	nbLSDEAD      = 6
	nbLSONPROC    = 7
	nbLSSUSPENDED = 8
)

// parseNetBSDProcs decodes a KERN_PROC2 blob (struct kinfo_proc2 per process). Args,
// Exe, and User are filled by collect_netbsd.go.
func parseNetBSDProcs(b []byte) []Proc {
	var out []Proc
	for off := 0; off+nbStride <= len(b); off += nbStride {
		r := b[off : off+nbStride]
		p := Proc{
			Pid:                    int32(le32(r, nbPid)),
			Ppid:                   int32(le32(r, nbPpid)),
			Uid:                    int32(le32(r, nbRuid)),
			Gid:                    int32(le32(r, nbRgid)),
			Name:                   cstr(r[nbComm : nbComm+nbComLen]),
			State:                  netbsdState(int8(r[nbStat])),
			Threads:                int32(le64(r, nbNlwps)),
			CreateTime:             usecMillis(int64(le32(r, nbStartSec)), int64(le32(r, nbStartUsec))),
			UserTime:               usecSeconds(int64(le32(r, nbUtimeSec)), int64(le32(r, nbUtimeUsec))),
			SystemTime:             usecSeconds(int64(le32(r, nbStimeSec)), int64(le32(r, nbStimeUsec))),
			VoluntaryCtxSwitches:   le64(r, nbNvcsw),
			InvoluntaryCtxSwitches: le64(r, nbNivcsw),
		}
		p.NsPid = p.Pid
		if p.Threads < 1 {
			p.Threads = 1 // the intake drops a process that reports zero threads
		}
		if pages := int64(int32(le32(r, nbRssize))); pages > 0 {
			p.Rss = uint64(pages) * pageSize
		}
		if vpages := int64(le64(r, nbVsize)); vpages > 0 {
			p.Vms = uint64(vpages) * pageSize
		}
		out = append(out, p)
	}
	return out
}

func netbsdState(st int8) ProcState {
	switch st {
	case nbLSRUN, nbLSONPROC:
		return stateR
	case nbLSSLEEP:
		return stateS
	case nbLSSTOP, nbLSSUSPENDED:
		return stateT
	case nbLSZOMB:
		return stateZ
	case nbLSDEAD:
		return stateX
	case nbLSIDL:
		return stateD
	default:
		return stateU
	}
}
