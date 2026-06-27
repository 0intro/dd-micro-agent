package host

func collectors(_ *Collector) []subCollector {
	return append(bsdCommon(), &vmstatsMem{}, &unixDisk{}, &freebsdIO{})
}
