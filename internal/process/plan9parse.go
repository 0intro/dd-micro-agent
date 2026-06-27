package process

// Pure parsers for Plan 9's text /proc files. No build constraint, so they
// unit-test on the dev host. collect_plan9.go (plan9-only) does the file I/O.

import "strings"

type plan9Status struct {
	name, user, state               string
	utimeMs, stimeMs, realMs, memKB uint64
}

// Plan 9 /proc/<pid>/status is a fixed-width line (sys/src/9/port/devproc.c,
// Qstatus): the program name and user in KNAMELEN(28)-byte space-padded columns,
// the state in a 12-byte column, then right-justified 12-byte numbers.
const (
	knamelen       = 28              // KNAMELEN, the name/user column width
	statusNumStart = 2*knamelen + 12 // the numeric columns begin after name, user, state
)

// parsePlan9Status reads /proc/<pid>/status. The three text columns are sliced at
// their fixed widths (a program name or user may itself contain a space, which a
// field split would let shift every later value), then the numeric remainder is
// field-split. The first six numbers are the process times in milliseconds
// (TUser, TSys, TReal and the three "children" variants). TReal is the wall-clock
// age of the process (the kernel writes MACHP(0)->ticks - p->time[TReal]), from
// which the collector derives the create time. The seventh number is the memory
// size in kB, followed by basepri and priority.
//
// Memory is anchored from the LEFT at the seventh number: both 9legacy kernels
// (9 and 9k) end the line after priority (nine numbers, where a right anchor
// would also land on memory), but the nix lineage appends three more counters
// (ntrap, nintr, nsyscall), where a right anchor lands on ntrap and, when that
// is zero, encodes an empty MemoryStat, omitted on the wire. Only the trailing
// columns vary between kernels. The leading layout, three text columns and six
// times before memory, is fixed.
func parsePlan9Status(data string) (plan9Status, bool) {
	if len(data) < statusNumStart {
		return plan9Status{}, false
	}
	f := strings.Fields(data[statusNumStart:])
	if len(f) < 9 {
		return plan9Status{}, false
	}
	return plan9Status{
		name:    strings.TrimSpace(data[:knamelen]),
		user:    strings.TrimSpace(data[knamelen : 2*knamelen]),
		state:   strings.TrimSpace(data[2*knamelen : statusNumStart]),
		utimeMs: atou(f[0]),
		stimeMs: atou(f[1]),
		realMs:  atou(f[2]),
		memKB:   atou(f[6]),
	}, true
}

// parsePlan9MemTotal returns total physical memory in bytes from the "<bytes>
// memory" line of /dev/swap (the kernel's first swapread row, sys/src/9/port/
// devswap.c). Mirrors hostmeta's parser of the same line.
func parsePlan9MemTotal(data string) uint64 {
	for _, line := range strings.Split(data, "\n") {
		f := strings.Fields(line)
		if len(f) == 2 && f[1] == "memory" {
			return atou(f[0])
		}
	}
	return 0
}

// parsePlan9Shared sums the size in bytes of a process's shared segments from
// /proc/<pid>/segment (sys/src/9/port/devproc.c Qsegment). Each line is
// "type perms base top ref": the perms column (an 'R' and/or 'P', or blank when
// neither) makes the leading field count vary, so the numbers are read from the
// right. ref (the last field) is the number of processes the segment is mapped
// into, base and top (the two before it) are its address range, all the same on
// the 386 kernel's %.8lux and the amd64 kernel's %p (both bare hex). A segment
// with ref > 1 is shared (the read-only text of a program run more than once, or
// an rfork(RFMEM) data/bss), so its virtual size counts toward MemoryStat.shared.
// This is a subset of the status memory the collector already reports as Rss/Vms.
func parsePlan9Shared(data string) uint64 {
	var shared uint64
	for _, line := range strings.Split(data, "\n") {
		f := strings.Fields(line)
		if len(f) < 4 { // need at least type, base, top, ref
			continue
		}
		if atou(f[len(f)-1]) <= 1 { // ref: 1 means mapped by this process alone
			continue
		}
		base, top := atoux(f[len(f)-3]), atoux(f[len(f)-2])
		if top > base {
			shared += top - base
		}
	}
	return shared
}

// plan9State maps the state word in /proc/<pid>/status to the wire enum. That
// word is either a scheduler state (statename[] in sys/src/9/port/proc.c) or,
// when the kernel set one, a finer "psstate" wait-channel name ("Fault", "Idle",
// "Pageout", …). Only Running/Ready/Scheding/New are actually on or bound for a
// CPU. Everything else is blocked, including the Queueing/QueueingR/QueueingW
// qlock waits, which sched() away (sys/src/9/port/qlock.c), and every psstate,
// which by definition names something the process is asleep waiting for. So the
// default is sleeping, not unknown: that keeps the state right for the majority
// of Plan 9 procs, which sit parked in some named wait.
func plan9State(s string) ProcState {
	switch s {
	case "Running", "Ready", "Scheding", "New":
		return stateR
	case "Stopped":
		return stateT
	case "Moribund", "Dead":
		return stateZ
	case "Broken":
		return stateX
	default: // Wakeme, Rendez, Waitrelease, Queueing*, and all psstate waits
		return stateS
	}
}

// parsePlan9Args splits /proc/<pid>/args into arguments. Plan 9 space-joins argv
// (quoting any arg that contains spaces). A field split is the pragmatic inverse.
func parsePlan9Args(data string) []string {
	return strings.Fields(data)
}

// parsePlan9Fds reads /proc/<pid>/fd (sys/src/9/port/devproc.c procfds): the first
// line is the process's working directory (p->dot->path), then one line per open
// file descriptor. So cwd is line 1 and the open-fd count is the number of lines
// after it.
func parsePlan9Fds(data string) (cwd string, nfd int) {
	for _, line := range strings.Split(data, "\n") {
		if line == "" {
			continue
		}
		if cwd == "" {
			cwd = line // first non-empty line: the working directory
			continue
		}
		nfd++ // each remaining line describes one open fd
	}
	return cwd, nfd
}
