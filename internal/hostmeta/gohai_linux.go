package hostmeta

import (
	"bufio"
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
)

// setOSVersion fills nixV = [platform, version, ""]. The lowercase platform in
// nixV[0] is what drives the OS icon. Use the os-release distro id ("ubuntu",
// "fedora", …) when available, falling back to runtime.GOOS ("linux") so a host
// without /etc/os-release still gets an icon, mirroring the stock Agent's
// gopsutil hostInfo.Platform.
func setOSVersion(s *systemStats) {
	id, ver := osRelease()
	if id == "" {
		id = runtime.GOOS
	}
	s.NixV = osVersion{id, ver, ""}
}

// osRelease reads ID and VERSION_ID from /etc/os-release.
func osRelease() (id, version string) {
	b, err := os.ReadFile("/etc/os-release")
	if err != nil {
		return "", ""
	}
	for _, line := range strings.Split(string(b), "\n") {
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		v = strings.Trim(v, "\"")
		switch k {
		case "ID":
			id = v
		case "VERSION_ID":
			version = v
		}
	}
	return id, version
}

// collectGohai gathers host facts from /proc and statfs (Linux).
func collectGohai(hostname string) gohai {
	return (&gohaiCollector{proc: "/proc", statfs: syscall.Statfs}).collect(hostname)
}

// gohaiCollector reads from a proc root and a statfs func, both injectable for
// tests.
type gohaiCollector struct {
	proc   string
	statfs func(path string, buf *syscall.Statfs_t) error
}

func (c *gohaiCollector) collect(hostname string) gohai {
	return gohai{
		Platform:   c.platform(hostname),
		CPU:        c.cpu(),
		Memory:     c.memory(),
		FileSystem: c.filesystem(),
		Network:    collectNetwork(),
	}
}

// platform reads kernel info from /proc/sys/kernel (avoiding syscall.Uname, whose
// field type differs by architecture) and the rest from the runtime.
func (c *gohaiCollector) platform(hostname string) gohaiPlatform {
	read := func(name string) string {
		b, err := os.ReadFile(filepath.Join(c.proc, "sys", "kernel", name))
		if err != nil {
			return ""
		}
		return strings.TrimSpace(string(b))
	}
	machine := map[string]string{"amd64": "x86_64", "arm64": "aarch64", "386": "i686", "arm": "armv7l"}[runtime.GOARCH]
	if machine == "" {
		machine = runtime.GOARCH
	}
	return gohaiPlatform{
		GoVersion:     runtime.Version(),
		GoOS:          runtime.GOOS,
		GoArch:        runtime.GOARCH,
		KernelName:    read("ostype"),
		KernelRelease: read("osrelease"),
		KernelVersion: read("version"),
		Hostname:      hostname,
		Machine:       machine,
		Processor:     machine,
		OS:            "GNU/Linux",
	}
}

func (c *gohaiCollector) cpu() gohaiCPU {
	data, err := os.ReadFile(filepath.Join(c.proc, "cpuinfo"))
	if err != nil {
		return gohaiCPU{}
	}
	var cpu gohaiCPU
	logical := 0
	cores := make(map[string]bool)
	physID, coreID := "", ""

	sc := bufio.NewScanner(bytes.NewReader(data))
	for sc.Scan() {
		key, val, ok := strings.Cut(sc.Text(), ":")
		if !ok {
			if physID != "" || coreID != "" { // blank line: end of a processor block
				cores[physID+"/"+coreID] = true
			}
			physID, coreID = "", ""
			continue
		}
		key, val = strings.TrimSpace(key), strings.TrimSpace(val)
		switch key {
		case "processor":
			logical++
		case "vendor_id":
			setOnce(&cpu.VendorID, val)
		case "model name":
			setOnce(&cpu.ModelName, val)
		case "cpu family":
			setOnce(&cpu.Family, val)
		case "model":
			setOnce(&cpu.Model, val)
		case "stepping":
			setOnce(&cpu.Stepping, val)
		case "cpu MHz":
			if cpu.Mhz == 0 {
				cpu.Mhz, _ = strconv.ParseFloat(val, 64)
			}
		case "physical id":
			physID = val
		case "core id":
			coreID = val
		}
	}
	if physID != "" || coreID != "" { // last block has no trailing blank line
		cores[physID+"/"+coreID] = true
	}

	cpu.CPULogicalProcessors = uint64(logical)
	if len(cores) > 0 {
		cpu.CPUCores = uint64(len(cores))
	} else {
		cpu.CPUCores = uint64(logical)
	}
	return cpu
}

func (c *gohaiCollector) memory() gohaiMemory {
	data, err := os.ReadFile(filepath.Join(c.proc, "meminfo"))
	if err != nil {
		return gohaiMemory{}
	}
	kb := make(map[string]uint64)
	sc := bufio.NewScanner(bytes.NewReader(data))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) < 2 {
			continue
		}
		if v, err := strconv.ParseUint(f[1], 10, 64); err == nil {
			kb[strings.TrimSuffix(f[0], ":")] = v
		}
	}
	return gohaiMemory{Total: kb["MemTotal"] * 1024, SwapTotal: kb["SwapTotal"]}
}

func (c *gohaiCollector) filesystem() []gohaiFS {
	data, err := os.ReadFile(filepath.Join(c.proc, "mounts"))
	if err != nil {
		return nil
	}
	out := []gohaiFS{}
	seen := make(map[string]bool)
	sc := bufio.NewScanner(bytes.NewReader(data))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) < 3 {
			continue
		}
		dev, mount, fstype := unescape(f[0]), unescape(f[1]), f[2]
		if pseudoFS[fstype] || seen[dev] {
			continue
		}
		var st syscall.Statfs_t
		if c.statfs(mount, &st) != nil || st.Blocks == 0 {
			continue
		}
		seen[dev] = true
		out = append(out, gohaiFS{Name: dev, SizeKB: st.Blocks * uint64(st.Bsize) / 1024, MountedOn: mount})
	}
	return out
}

func setOnce(dst *string, val string) {
	if *dst == "" {
		*dst = val
	}
}

// pseudoFS lists filesystems with no real disk to report (kernel virtual FS and
// read-only image mounts). Kept in sync with internal/host's exclusion set.
var pseudoFS = map[string]bool{
	"autofs": true, "binfmt_misc": true, "bpf": true, "cgroup": true,
	"cgroup2": true, "configfs": true, "debugfs": true, "devpts": true,
	"devtmpfs": true, "fusectl": true, "hugetlbfs": true, "iso9660": true,
	"mqueue": true, "nsfs": true, "overlay": true, "proc": true,
	"pstore": true, "ramfs": true, "securityfs": true, "squashfs": true,
	"sysfs": true, "tmpfs": true, "tracefs": true,
}

// unescape resolves the octal escapes (\040 space, \011 tab, \012 newline,
// \134 backslash) that /proc/mounts uses for those characters in a path. A
// copy of its twin in internal/host's fs_linux.go, kept local because host and
// hostmeta deliberately do not import each other.
func unescape(s string) string {
	if !strings.ContainsRune(s, '\\') {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+3 < len(s) {
			if c, ok := octal(s[i+1], s[i+2], s[i+3]); ok {
				b.WriteByte(c)
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

func octal(a, b, c byte) (byte, bool) {
	if a < '0' || a > '7' || b < '0' || b > '7' || c < '0' || c > '7' {
		return 0, false
	}
	return (a-'0')<<6 | (b-'0')<<3 | (c - '0'), true
}
