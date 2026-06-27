package process

// DragonFly process decoding. parseDragonFlyProcs unpacks a kern.proc.all sysctl blob, an
// array of fixed-size struct kinfo_proc (sys/kinfo.h), into Procs. DragonFly's kinfo_proc
// has no self-describing size field (unlike FreeBSD's ki_structsize), so the iteration
// stride is the fixed record size. Each record carries the process fields plus an embedded
// struct kinfo_lwp (the representative thread), whose kl_stat refines the coarse kp_stat.
// The offsets are pinned for the amd64 ABI and verified against a live kern.proc.all blob
// by the vm_dragonfly e2e, which compiles an offdump.c and prints them (DragonFly 6.4.x,
// where sizeof(struct kinfo_proc) is 992). This file carries no build tag so the parser
// unit-tests on the dev host. collect_dragonfly.go (dragonfly-only) makes the sysctl call.

import "encoding/binary"

// Field offsets within one struct kinfo_proc (DragonFly amd64) plus the record stride.
// dfUtime..dfNivcsw are absolute offsets into the embedded kp_ru rusage, dfLwpStat into
// the embedded kp_lwp.
const (
	dfStride    = 992 // sizeof(struct kinfo_proc)
	dfStat      = 12  // enum procstat  kp_stat                -> State
	dfStart     = 96  // struct timeval kp_start               -> CreateTime
	dfComm      = 112 // char           kp_comm[MAXCOMLEN+1]
	dfCommLen   = 17  // MAXCOMLEN+1
	dfRuid      = 204 // uid_t          kp_ruid
	dfRgid      = 212 // gid_t          kp_rgid
	dfPid       = 220 // pid_t          kp_pid
	dfPpid      = 224 // pid_t          kp_ppid
	dfNthreads  = 296 // int            kp_nthreads
	dfVmMapSize = 312 // size_t         kp_vm_map_size (bytes) -> Vms
	dfVmRssize  = 320 // segsz_t        kp_vm_rssize   (pages) -> Rss
	dfUtime     = 368 // struct timeval kp_ru.ru_utime
	dfStime     = 384 // struct timeval kp_ru.ru_stime
	dfNvcsw     = 496 // long           kp_ru.ru_nvcsw
	dfNivcsw    = 504 // long           kp_ru.ru_nivcsw
	dfLwpStat   = 676 // enum lwpstat   kp_lwp.kl_stat         -> refines State
)

// enum procstat and enum lwpstat values (sys/proc_common.h).
const (
	dfSIDL    = 1 // being created
	dfSACTIVE = 2 // runnable or sleeping (kl_stat distinguishes)
	dfSSTOP   = 3 // stopped
	dfSZOMB   = 4 // zombie
	dfSCORE   = 5 // dumping core

	dfLSRUN   = 1 // on a run queue
	dfLSSTOP  = 2 // stopped
	dfLSSLEEP = 3 // sleeping
)

// parseDragonFlyProcs decodes the kern.proc.all blob (one fixed-size struct kinfo_proc per
// process). Args/Exe/User are not in kinfo_proc and are filled by collect_dragonfly.go.
func parseDragonFlyProcs(b []byte) []Proc {
	var out []Proc
	for off := 0; off+dfStride <= len(b); off += dfStride {
		r := b[off : off+dfStride]
		p := Proc{
			Pid:                    int32(binary.LittleEndian.Uint32(r[dfPid:])),
			Ppid:                   int32(binary.LittleEndian.Uint32(r[dfPpid:])),
			Uid:                    int32(binary.LittleEndian.Uint32(r[dfRuid:])),
			Gid:                    int32(binary.LittleEndian.Uint32(r[dfRgid:])),
			Name:                   cstr(r[dfComm : dfComm+dfCommLen]),
			State:                  dragonflyState(r),
			Threads:                int32(binary.LittleEndian.Uint32(r[dfNthreads:])),
			Vms:                    binary.LittleEndian.Uint64(r[dfVmMapSize:]),
			CreateTime:             tvMillis(r, dfStart),
			UserTime:               tvSeconds(r, dfUtime),
			SystemTime:             tvSeconds(r, dfStime),
			VoluntaryCtxSwitches:   binary.LittleEndian.Uint64(r[dfNvcsw:]),
			InvoluntaryCtxSwitches: binary.LittleEndian.Uint64(r[dfNivcsw:]),
		}
		p.NsPid = p.Pid
		// A DragonFly proc is scheduled as one or more LWPs. The intake drops a process
		// reporting zero threads, so treat an unset count as one (a kernel proc reports 0).
		if p.Threads < 1 {
			p.Threads = 1
		}
		// kp_vm_rssize is in pages (segsz_t, signed). Clamp a stray negative so it can't
		// become a huge unsigned RSS.
		if pages := int64(binary.LittleEndian.Uint64(r[dfVmRssize:])); pages > 0 {
			p.Rss = uint64(pages) * pageSize
		}
		out = append(out, p)
	}
	return out
}

// dragonflyState maps the coarse kp_stat, refined for an active process by the
// representative LWP's kl_stat, to the wire enum.
func dragonflyState(r []byte) ProcState {
	switch int32(binary.LittleEndian.Uint32(r[dfStat:])) {
	case dfSIDL:
		return stateD
	case dfSSTOP:
		return stateT
	case dfSZOMB:
		return stateZ
	case dfSCORE:
		return stateR
	case dfSACTIVE:
		switch int32(binary.LittleEndian.Uint32(r[dfLwpStat:])) {
		case dfLSRUN:
			return stateR
		case dfLSSTOP:
			return stateT
		default: // LSSLEEP
			return stateS
		}
	default:
		return stateU
	}
}
