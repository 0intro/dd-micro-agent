package process

// Encoding of the Datadog process-intake messages we send (CollectorProc and
// CollectorRealTime) plus the agent-payload framing. Field numbers and message
// types are from github.com/DataDog/agent-payload/v5/process (proto3). They are a
// stable wire contract, so we pin them here rather than depend on the module.

// agent-payload MessageV3 header bytes and message-type tags.
const (
	messageV3        = 3
	encodingProtobuf = 0 // plain, uncompressed protobuf (the Agent's default is zstd)

	typeCollectorProc     = 12
	typeResCollector      = 23
	typeCollectorRealTime = 27
)

// frame wraps a protobuf body in the 16-byte MessageV3 header. Layout
// (little-endian): version(1), encoding(1), type(1), subscriptionID(1),
// orgID(int32), timestamp(int64). The stock Agent sets only version/encoding/type
// (pkg/process/util/api/payload.go:43) and leaves the rest zero, so we do too.
func frame(typ int, body []byte) []byte {
	out := make([]byte, 16+len(body))
	out[0] = messageV3
	out[1] = encodingProtobuf
	out[2] = byte(typ)
	// bytes 3..15 (subscriptionID, orgID, timestamp) stay zero
	copy(out[16:], body)
	return out
}

// unframe splits a framed message into body and type. ok is false if truncated.
func unframe(framed []byte) (body []byte, typ int, ok bool) {
	if len(framed) < 16 {
		return nil, 0, false
	}
	return framed[16:], int(framed[2]), true
}

// ProcState is the process.ProcessState enum (the wire integers).
type ProcState int32

const (
	stateU ProcState = iota // unknown / unset
	stateD                  // uninterruptible sleep
	stateR                  // running or runnable
	stateS                  // sleeping
	stateT                  // stopped / traced
	stateW                  // paging
	stateX                  // dead
	stateZ                  // zombie
)

// Proc is one process, normalised across operating systems. Fields an OS cannot
// supply are left zero. The intake tolerates absent fields. Cumulative counters
// (CPU times, IO bytes) feed the Reporter's per-PID rate diffing, which fills the
// *Pct/*Rate fields before encoding.
type Proc struct {
	Pid, Ppid, NsPid int32
	Name             string // command name (comm)
	Exe              string // executable path or name
	Cwd              string
	Args             []string
	User             string
	Uid, Gid         int32
	State            ProcState
	CreateTime       int64 // epoch milliseconds, 0 if unknown
	Threads          int32
	OpenFd           int32 // 0 if unavailable

	// memory, bytes
	Rss, Vms, Swap, Shared uint64

	// CPU: cumulative seconds (for diffing) + the percentages the Reporter computes
	UserTime, SystemTime         float64
	UserPct, SystemPct, TotalPct float32

	// IO: cumulative counts/bytes (for diffing) + the rates the Reporter computes
	ReadCount, WriteCount, ReadBytes, WriteBytes       uint64
	ReadRate, WriteRate, ReadBytesRate, WriteBytesRate float32

	VoluntaryCtxSwitches, InvoluntaryCtxSwitches uint64
}

// sysInfo is the host system metadata embedded in CollectorProc.Info.
type sysInfo struct {
	os, platform string
	totalMemory  int64
	numCPU       int
}

func encodeCommand(p *Proc) []byte {
	var w pbuf
	for _, a := range p.Args {
		w.str(1, a) // repeated Args
	}
	w.str(3, p.Cwd)
	w.uint(6, uint64(p.Ppid))
	w.str(8, p.Exe)
	w.str(9, p.Name) // Comm
	return w.b
}

func encodeUser(p *Proc) []byte {
	var w pbuf
	w.str(1, p.User)
	w.uint(2, uint64(uint32(p.Uid)))
	w.uint(3, uint64(uint32(p.Gid)))
	return w.b
}

func encodeMemory(p *Proc) []byte {
	var w pbuf
	w.uint(1, p.Rss)
	w.uint(2, p.Vms)
	w.uint(3, p.Swap)
	w.uint(4, p.Shared)
	return w.b
}

func encodeCPU(p *Proc) []byte {
	var w pbuf
	w.f32(2, p.TotalPct)
	w.f32(3, p.UserPct)
	w.f32(4, p.SystemPct)
	w.uint(5, uint64(p.Threads))
	w.uint(8, uint64(p.UserTime))
	w.uint(9, uint64(p.SystemTime))
	return w.b
}

func encodeIO(p *Proc) []byte {
	var w pbuf
	w.f32(1, p.ReadRate)
	w.f32(2, p.WriteRate)
	w.f32(3, p.ReadBytesRate)
	w.f32(4, p.WriteBytesRate)
	return w.b
}

// encodeProcess renders a Process message (used in CollectorProc).
func encodeProcess(p *Proc) []byte {
	var w pbuf
	w.uint(2, uint64(p.Pid))
	w.msg(4, encodeCommand(p))
	w.msg(5, encodeUser(p))
	w.msg(7, encodeMemory(p))
	w.msg(8, encodeCPU(p))
	w.uint(9, uint64(p.CreateTime))
	w.uint(11, uint64(p.OpenFd))
	w.uint(12, uint64(p.State))
	w.msg(13, encodeIO(p))
	w.uint(16, p.VoluntaryCtxSwitches)
	w.uint(17, p.InvoluntaryCtxSwitches)
	w.uint(20, uint64(p.NsPid))
	return w.b
}

// encodeProcessStat renders a ProcessStat message (used in CollectorRealTime).
// It carries the per-cycle changing stats. Identity (command, user) is omitted,
// the backend correlates by Pid+CreateTime against the last CollectorProc.
func encodeProcessStat(p *Proc) []byte {
	var w pbuf
	w.uint(1, uint64(p.Pid))
	w.uint(2, uint64(p.CreateTime))
	w.msg(3, encodeMemory(p))
	w.msg(4, encodeCPU(p))
	w.uint(7, uint64(p.Threads))
	w.uint(8, uint64(p.OpenFd))
	w.uint(12, uint64(p.State))
	w.msg(19, encodeIO(p))
	w.uint(24, p.VoluntaryCtxSwitches)
	w.uint(25, p.InvoluntaryCtxSwitches)
	return w.b
}

func encodeOSInfo(s sysInfo) []byte {
	var w pbuf
	w.str(1, s.os)
	w.str(2, s.platform)
	return w.b
}

func encodeSystemInfo(s sysInfo) []byte {
	var w pbuf
	w.msg(2, encodeOSInfo(s))
	w.uint(5, uint64(s.totalMemory))
	return w.b
}

// encodeCollectorProc renders one CollectorProc chunk (a HostName, the host
// SystemInfo, a slice of processes, and the group identity for reassembly).
func encodeCollectorProc(hostname string, info sysInfo, procs []*Proc, groupID, groupSize int32) []byte {
	var w pbuf
	w.str(2, hostname)
	for _, p := range procs {
		w.msg(3, encodeProcess(p))
	}
	w.msg(5, encodeSystemInfo(info))
	w.uint(6, uint64(uint32(groupID)))
	w.uint(7, uint64(uint32(groupSize)))
	return w.b
}

// encodeCollectorRealTime renders one CollectorRealTime chunk.
func encodeCollectorRealTime(hostname string, stats []*Proc, groupID, groupSize int32, numCPU int, totalMem int64) []byte {
	var w pbuf
	w.str(2, hostname)
	for _, p := range stats {
		w.msg(3, encodeProcessStat(p))
	}
	w.uint(6, uint64(uint32(groupID)))
	w.uint(7, uint64(uint32(groupSize)))
	w.uint(8, uint64(numCPU))
	w.uint(9, uint64(totalMem))
	return w.b
}
