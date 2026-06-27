//go:build openbsd || netbsd

// Command proccap dumps the raw KERN_PROC/KERN_PROC2 sysctl blob the process
// collector decodes, plus the record stride on stderr. The vm_openbsd and vm_netbsd
// e2e tests run it in the guest to capture the live struct layout (the analog of
// vm_freebsd dumping kern.proc.all), so the parser offsets can be checked against
// the exact release. It uses the collector's own RawProcBlob, so the capture cannot
// drift from what the agent reads.
package main

import (
	"fmt"
	"os"

	"github.com/0intro/dd-micro-agent/internal/process"
)

func main() {
	b, stride, err := process.RawProcBlob()
	if err != nil {
		fmt.Fprintln(os.Stderr, "proccap:", err)
		os.Exit(1)
	}
	rem := len(b) % stride
	fmt.Fprintf(os.Stderr, "stride=%d bytes=%d records=%d remainder=%d\n", stride, len(b), len(b)/stride, rem)
	if _, err := os.Stdout.Write(b); err != nil {
		fmt.Fprintln(os.Stderr, "proccap write:", err)
		os.Exit(1)
	}
}
