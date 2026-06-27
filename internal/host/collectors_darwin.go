package host

// collectors returns the macOS sub-collectors achievable in pure Go: load
// (vm.loadavg), uptime (kern.boottime), and disk (getfsstat). CPU% and
// memory-usage need Mach host_statistics (cgo) and are intentionally omitted.
// Memory total is still reported as host info via hostmeta.
func collectors(_ *Collector) []subCollector {
	return []subCollector{&bsdLoad{}, &bsdUptime{}, &unixDisk{}}
}
