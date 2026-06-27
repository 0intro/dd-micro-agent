//go:build !linux && !darwin && !freebsd && !openbsd && !netbsd && !windows && !plan9 && !dragonfly

package hostmeta

import "runtime"

// collectGohai returns a minimal payload on platforms without a dedicated
// collector. Host info still carries the OS name, hostname, and network.
func collectGohai(hostname string) gohai {
	return gohai{
		Platform: gohaiPlatform{
			GoVersion: runtime.Version(),
			GoOS:      runtime.GOOS,
			GoArch:    runtime.GOARCH,
			Hostname:  hostname,
			Machine:   runtime.GOARCH,
			OS:        runtime.GOOS,
		},
		Network:    collectNetwork(),
		FileSystem: []gohaiFS{},
	}
}

func setOSVersion(*systemStats) {}
