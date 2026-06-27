// Command parity is the local fake intake + comparison engine for the
// micro-agent vs. stock Agent parity test (e2e/parity.sh). Both agents
// are pointed at this recorder instead of real Datadog. The recordings are then
// diffed. Stdlib only and in-repo, so the comparison logic unit-tests on the dev
// host (compare_test.go) and the tool builds static like the agent.
//
//	parity serve -dir DIR LABEL=ADDR [LABEL=ADDR ...]
//	    Record decoded payloads per label to DIR/<label>.jsonl until SIGINT/SIGTERM.
//	parity compare [-platform darwin] OURS.jsonl STOCK.jsonl
//	    Diff the two recordings, print a tiered report, and exit non-zero on any
//	    Tier-1 mismatch or Tier-2 hard failure. -platform darwin skips the Live
//	    Processes tier (the stock macOS Agent ships no process-agent).
//	parity verify [flags] RECORDING.jsonl
//	    Assert one recording contains the expected records (series, host metadata,
//	    processes, logs), for the per-OS VM e2e against the fake intake with no
//	    stock agent. Exit non-zero on any miss.
package main

import (
	"flag"
	"fmt"
	"os"
)

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	switch os.Args[1] {
	case "serve":
		serve(os.Args[2:])
	case "compare":
		os.Exit(runCompare(os.Args[2:]))
	case "verify":
		os.Exit(verify(os.Args[2:]))
	default:
		usage()
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: parity serve -dir DIR LABEL=ADDR... | parity compare OURS.jsonl STOCK.jsonl | parity verify [flags] RECORDING.jsonl")
	os.Exit(2)
}

func runCompare(args []string) int {
	fs := flag.NewFlagSet("compare", flag.ExitOnError)
	platform := fs.String("platform", "", "guest platform; 'darwin' skips the Live Processes tier (stock macOS has no process-agent)")
	fs.Parse(args)
	if fs.NArg() != 2 {
		fmt.Fprintln(os.Stderr, "usage: parity compare [-platform darwin] OURS.jsonl STOCK.jsonl")
		return 2
	}
	ours, err := loadRecords(fs.Arg(0))
	if err != nil {
		fmt.Fprintf(os.Stderr, "load %s: %v\n", fs.Arg(0), err)
		return 2
	}
	stock, err := loadRecords(fs.Arg(1))
	if err != nil {
		fmt.Fprintf(os.Stderr, "load %s: %v\n", fs.Arg(1), err)
		return 2
	}
	report, pass := compareForPlatform(ours, stock, *platform)
	fmt.Print(report)
	if pass {
		fmt.Println("==> PARITY PASS")
		return 0
	}
	fmt.Println("==> PARITY FAIL")
	return 1
}
