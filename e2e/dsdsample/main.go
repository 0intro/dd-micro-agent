// Command dsdsample simulates a service that emits a realistic DogStatsD
// workload (a gauge, a counter, a histogram, a timing, and a set, plus one
// service check and one event) so an end-to-end test exercises the whole
// metrics forwarding pipeline (DogStatsD parse → aggregate → series / check_run
// / events intakes), not just a single gauge.
//
// It is stdlib only and writes the wire format straight to a UDP socket, exactly
// as a real DogStatsD client does, so it adds no module dependency and builds
// static like the agent. Used by e2e/vm_linux.sh.
package main

import (
	"flag"
	"fmt"
	"net"
	"os"
	"strings"
	"time"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:8125", "comma-separated DogStatsD UDP address(es); each datagram goes to all")
	dur := flag.Duration("duration", 20*time.Second, "how long to emit (should span an agent flush)")
	every := flag.Duration("every", time.Second, "interval between emission cycles")
	tags := flag.String("tags", "", "comma-separated tags added to every submission")
	prefix := flag.String("prefix", "microagent.vm.dsd", "metric-name prefix")
	flag.Parse()

	// Fan every datagram out to one or more listeners. Two agents running side by
	// side bind different ports. Sending the identical byte stream to both is what
	// makes a parity comparison meaningful (no per-process cycle-count skew).
	var conns []net.Conn
	for _, a := range strings.Split(*addr, ",") {
		c, err := net.Dial("udp", a)
		if err != nil {
			fmt.Fprintf(os.Stderr, "dsdsample: dial %s: %v\n", a, err)
			os.Exit(1)
		}
		defer c.Close()
		conns = append(conns, c)
	}

	// Every line carries the same tag set (DogStatsD appends them as |#a,b). The
	// e2e filters its queries on these, so they scope verification to this run.
	tagSuffix := ""
	if *tags != "" {
		tagSuffix = "|#" + *tags
	}
	send := func(line string) {
		for _, c := range conns {
			if _, err := c.Write([]byte(line + tagSuffix + "\n")); err != nil {
				fmt.Fprintf(os.Stderr, "dsdsample: write: %v\n", err)
			}
		}
	}

	// One service check and one event up front: they exercise the _sc/_e parse and
	// the check_run / events forwarding paths (their delivery is asserted via the
	// agent's debug log, like e2e.sh, because pup can't query them back cleanly).
	send("_sc|" + *prefix + ".check|0")
	title, text := "dsdsample up", "emitting a dogstatsd workload"
	send(fmt.Sprintf("_e{%d,%d}:%s|%s|t:info", len(title), len(text), title, text))

	// Each cycle emits a FIXED multiset, so a histogram/set aggregate over any flush
	// window is the same regardless of the window's phase. Two agents started a
	// moment apart still produce identical aggregates. Counters (a rate) instead
	// match on their run total, which is likewise phase-independent.
	hist := []int{1, 5, 10} // → min 1, median 5, avg 16/3, max 10, p95 10 in every window
	timing := []int{10, 50, 90}
	users := []string{"alice", "bob", "carol", "dave", "erin"} // 5 distinct, every cycle

	// The whole cycle rides in one datagram (well under the 64 KiB UDP ceiling),
	// so packet loss is cycle-atomic and any flush window still holds the fixed
	// multiset the aggregate comparison relies on.
	var cycle strings.Builder
	line := func(f string, a ...any) {
		fmt.Fprintf(&cycle, f, a...)
		cycle.WriteString(tagSuffix)
		cycle.WriteByte('\n')
	}
	line("%s.gauge:42|g", *prefix)   // gauge → constant value
	line("%s.requests:1|c", *prefix) // counter → rate, compared by run total
	for _, v := range hist {
		line("%s.render:%d|h", *prefix, v) // histogram → .avg/.median/.max/.95percentile/.count
	}
	for _, v := range timing {
		line("%s.latency:%d|ms", *prefix, v) // timing → expands like a histogram
	}
	for _, u := range users {
		line("%s.users:%s|s", *prefix, u) // set → distinct-member count (5)
	}
	payload := []byte(cycle.String())

	deadline := time.Now().Add(*dur)
	cycles := 0
	for time.Now().Before(deadline) {
		for _, c := range conns {
			if _, err := c.Write(payload); err != nil {
				fmt.Fprintf(os.Stderr, "dsdsample: write: %v\n", err)
			}
		}
		cycles++
		time.Sleep(*every)
	}
	fmt.Fprintf(os.Stderr, "dsdsample: emitted %d cycles over %s to %s\n", cycles, *dur, *addr)
}
