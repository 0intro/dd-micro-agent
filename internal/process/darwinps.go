package process

// Pure parsers for the macOS collector, which (lacking a non-cgo path to thread
// counts and reliable RSS via kinfo_proc) shells out to ps, the same approach
// gopsutil takes on darwin. These carry no build constraint, so they unit-test on
// the dev host against captured ps output. collect_darwin.go (darwin-only) runs ps.

import (
	"path"
	"strconv"
	"strings"
)

// parsePsProcs parses the output of
//
//	ps -axww -o pid,ppid,uid,gid,rss,vsz,state,time,etime,user,command
//
// The first ten columns are single tokens. command (everything after) may contain
// spaces, so it is taken as the remainder. A header line (or anything whose first
// field isn't a PID) is skipped. nowMs converts the elapsed time to a create time.
// Each proc defaults to Threads=1 so it renders even if the thread-count pass below
// fails. The backend drops zero-thread processes.
func parsePsProcs(out string, nowMs int64) []Proc {
	var procs []Proc
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) < 11 {
			continue
		}
		pid, err := strconv.Atoi(f[0])
		if err != nil || pid <= 0 { // skips the header row and blanks
			continue
		}
		command := strings.Join(f[10:], " ")
		p := Proc{
			Pid:     int32(pid),
			NsPid:   int32(pid),
			Ppid:    atoi32(f[1]),
			Uid:     atoi32(f[2]),
			Gid:     atoi32(f[3]),
			User:    f[9],
			Rss:     atou(f[4]) * 1024, // ps rss/vsz are in kB
			Vms:     atou(f[5]) * 1024,
			State:   psState(f[6]),
			Threads: 1,
			// `time` is cumulative CPU. We get no user/sys split without utime/stime
			// (whose ps keyword support varies), so it all lands in UserTime. The
			// Reporter's diff still yields a correct TotalPct.
			UserTime: parsePsCPUSeconds(f[7]),
		}
		if secs := parsePsElapsedSeconds(f[8]); secs > 0 {
			p.CreateTime = nowMs - secs*1000
		}
		args := strings.Fields(command) // non-empty: len(f) >= 11 guarantees a command
		p.Args, p.Exe, p.Name = args, args[0], path.Base(args[0])
		procs = append(procs, p)
	}
	return procs
}

// parsePsThreads counts threads per PID from `ps -axM`, which prints one line
// per thread: the first carries the process columns ("USER PID ..."), the rest
// leave them blank. A line whose second field is a PID therefore starts a new
// process and is its first thread. The exact -M layout varies by macOS version,
// so this is best-effort. collect_darwin keeps the default Threads=1 for any
// PID this doesn't resolve.
func parsePsThreads(out string) map[int32]int {
	counts := make(map[int32]int)
	var cur int32
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) < 2 {
			continue
		}
		if pid, err := strconv.Atoi(f[1]); err == nil && pid > 0 {
			cur = int32(pid)
			counts[cur] = 1 // the process line is the first thread
			continue
		}
		if cur != 0 {
			counts[cur]++ // a continuation line is one more thread
		}
	}
	return counts
}

// psState maps a macOS ps STAT code (first char) to the wire enum.
func psState(s string) ProcState {
	if s == "" {
		return stateU
	}
	switch s[0] {
	case 'R':
		return stateR
	case 'S', 'I': // sleeping, idle (sleeping >20s)
		return stateS
	case 'T':
		return stateT
	case 'U': // uninterruptible wait
		return stateD
	case 'Z':
		return stateZ
	default:
		return stateS
	}
}

// parsePsCPUSeconds parses a ps `time` field ("MM:SS.ss" or "H:MM:SS").
func parsePsCPUSeconds(s string) float64 {
	parts := strings.Split(s, ":")
	var secs float64
	for _, p := range parts {
		v, _ := strconv.ParseFloat(p, 64)
		secs = secs*60 + v
	}
	return secs
}

// parsePsElapsedSeconds parses a ps `etime` field: "[[DD-]HH:]MM:SS".
func parsePsElapsedSeconds(s string) int64 {
	days := int64(0)
	if d, rest, ok := strings.Cut(s, "-"); ok {
		days, _ = strconv.ParseInt(d, 10, 64)
		s = rest
	}
	var secs int64
	for _, p := range strings.Split(s, ":") {
		v, _ := strconv.ParseInt(p, 10, 64)
		secs = secs*60 + v
	}
	return days*86400 + secs
}
