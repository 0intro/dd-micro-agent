//go:build !linux && !darwin && !windows && !freebsd && !openbsd && !netbsd && !plan9 && !dragonfly

package host

// collectors returns no sub-collectors on platforms without a dedicated
// implementation (e.g. solaris, illumos). The host still reports its
// info via hostmeta. Only stats are absent here.
func collectors(*Collector) []subCollector { return nil }
