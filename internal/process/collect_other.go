//go:build !linux && !plan9 && !darwin && !windows && !freebsd && !openbsd && !netbsd && !dragonfly

package process

// collectProcs has no implementation on the remaining platforms (Solaris, AIX, and so on),
// so the reporter runs but reports nothing. Linux, Plan 9, macOS, Windows, FreeBSD,
// OpenBSD, NetBSD, and DragonFly each have a real collect_<goos>.go.
func collectProcs() ([]Proc, error) { return nil, nil }

// hostTotalMemory is unavailable here too (these platforms collect no processes, so the
// field is moot).
func hostTotalMemory() int64 { return 0 }
