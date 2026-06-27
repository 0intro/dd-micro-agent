package hostmeta

// Pure parsers for the Plan 9 host facts read from /dev. No build constraint (the
// name has no _GOOS suffix), so they compile and are unit-tested on the dev host,
// gohai_plan9.go (plan9 only) opens the real files and calls these.

import (
	"strconv"
	"strings"
)

// parsePlan9Cputype parses /dev/cputype, whose format (sys/src/9/pc/devarch.c
// cputyperead: snprint "%s %lud") is "<name> <mhz>". The name itself often
// contains spaces (the X86type table has entries like "Core i7/Xeon", "AMD
// Geode GX1", "AMD-K10 Opteron G34", "P55C MMX"), so the MHz is the LAST field
// and the model is everything before it. Splitting on the first space would
// truncate the model and lose the MHz on most modern CPUs.
func parsePlan9Cputype(data string) (model string, mhz float64) {
	f := strings.Fields(data)
	if len(f) == 0 {
		return "", 0
	}
	if len(f) >= 2 {
		if v, err := strconv.ParseFloat(f[len(f)-1], 64); err == nil {
			return strings.Join(f[:len(f)-1], " "), v
		}
	}
	// No trailing numeric MHz (e.g. a name-only file): take it all as the model.
	return strings.Join(f, " "), 0
}

// parsePlan9MemTotal returns total physical memory in bytes from the
// "<bytes> memory" line of /dev/swap.
func parsePlan9MemTotal(data string) uint64 {
	for _, line := range strings.Split(data, "\n") {
		f := strings.Fields(line)
		if len(f) == 2 && f[1] == "memory" {
			v, _ := strconv.ParseUint(f[0], 10, 64)
			return v
		}
	}
	return 0
}

// countPlan9CPUs counts the per-CPU rows of /dev/sysstat (one line per online CPU,
// each has ten columns, the first being the numeric CPU id).
func countPlan9CPUs(data string) int {
	n := 0
	for _, line := range strings.Split(data, "\n") {
		f := strings.Fields(line)
		if len(f) >= 10 {
			if _, err := strconv.Atoi(f[0]); err == nil {
				n++
			}
		}
	}
	return n
}
