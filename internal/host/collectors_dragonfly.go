package host

// DragonFly host stats reuse the shared BSD collectors: cpu (kern.cp_time),
// load (vm.loadavg), and uptime (kern.boottime) from bsdCommon, memory from the
// vm.stats path shared with FreeBSD, disk usage from getfsstat, and disk I/O from
// kern.devstat.all (throughput only, see devstatdf.go). Network is absent, as on
// the other BSDs.
func collectors(_ *Collector) []subCollector {
	return append(bsdCommon(), &vmstatsMem{}, &unixDisk{}, &dragonflyIO{})
}
