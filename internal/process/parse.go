package process

// Small numeric helpers shared by the per-OS collectors. This file carries no
// build constraint, so it compiles everywhere (and the parsers that use it stay
// unit-testable on the dev host).

import "strconv"

// clockTicks converts Linux's cumulative tick counters (USER_HZ) to seconds.
// sysconf(_SC_CLK_TCK) needs cgo, but 100 is the universal value, as the host
// package also assumes.
const clockTicks = 100

func atou(s string) uint64  { n, _ := strconv.ParseUint(s, 10, 64); return n }
func atoi32(s string) int32 { n, _ := strconv.ParseInt(s, 10, 32); return int32(n) }

// atoux parses an unsigned hex number, tolerating an optional "0x" prefix. Plan 9
// prints addresses as bare hex (the kernel's %.8lux and %p both omit the prefix),
// so the strip is just insurance against a variant that adds one.
func atoux(s string) uint64 {
	if len(s) >= 2 && s[0] == '0' && (s[1] == 'x' || s[1] == 'X') {
		s = s[2:]
	}
	n, _ := strconv.ParseUint(s, 16, 64)
	return n
}

// parsePid reads a /proc entry name as a positive PID. ok is false for the
// non-process entries (e.g. "self", "stat").
func parsePid(s string) (int32, bool) {
	n, err := strconv.Atoi(s)
	if err != nil || n <= 0 {
		return 0, false
	}
	return int32(n), true
}
