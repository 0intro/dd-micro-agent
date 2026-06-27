package host

import (
	"bufio"
	"bytes"
	"strings"
	"time"

	"github.com/0intro/dd-micro-agent/internal/metrics"
)

// netCollector reports per-interface throughput as per-second rates, diffing the
// cumulative counters in /proc/net/dev between reads. The loopback interface is
// skipped. The first read only establishes the baseline.
type netCollector struct {
	c      *Collector
	prev   map[string]netCounters
	prevTs time.Time
}

type netCounters struct {
	rxBytes, rxPackets, rxErrs, rxDrop float64
	txBytes, txPackets, txErrs, txDrop float64
}

func (nc *netCollector) name() string { return "network" }

func (nc *netCollector) collect(now time.Time) ([]metrics.Serie, error) {
	data, err := nc.c.readProc("net/dev")
	if err != nil {
		return nil, err
	}
	cur := parseNetDev(data)

	var out []metrics.Serie
	if nc.prev != nil {
		if dt := now.Sub(nc.prevTs).Seconds(); dt > 0 {
			for iface, c := range cur {
				p, ok := nc.prev[iface]
				if !ok {
					continue
				}
				dev := "device:" + iface
				rate := func(name string, cur, prev float64) {
					if d := cur - prev; d >= 0 {
						out = append(out, gauge(name, now, d/dt, dev))
					}
				}
				rate("system.net.bytes_rcvd", c.rxBytes, p.rxBytes)
				rate("system.net.bytes_sent", c.txBytes, p.txBytes)
				rate("system.net.packets_in.count", c.rxPackets, p.rxPackets)
				rate("system.net.packets_in.error", c.rxErrs, p.rxErrs)
				rate("system.net.packets_in.drop", c.rxDrop, p.rxDrop)
				rate("system.net.packets_out.count", c.txPackets, p.txPackets)
				rate("system.net.packets_out.error", c.txErrs, p.txErrs)
				rate("system.net.packets_out.drop", c.txDrop, p.txDrop)
			}
		}
	}
	nc.prev = cur
	nc.prevTs = now
	return out, nil
}

// parseNetDev parses /proc/net/dev. Each data line is "iface: rx... tx...":
// receive columns are bytes packets errs drop fifo frame compressed multicast,
// transmit columns follow in the same order.
func parseNetDev(data []byte) map[string]netCounters {
	out := make(map[string]netCounters)
	sc := bufio.NewScanner(bytes.NewReader(data))
	for sc.Scan() {
		line := sc.Text()
		name, rest, ok := strings.Cut(line, ":")
		if !ok {
			continue // header lines have no colon
		}
		iface := strings.TrimSpace(name)
		if iface == "" || iface == "lo" {
			continue
		}
		f := parseFloats(strings.Fields(rest))
		if len(f) < 16 {
			continue
		}
		out[iface] = netCounters{
			rxBytes: f[0], rxPackets: f[1], rxErrs: f[2], rxDrop: f[3],
			txBytes: f[8], txPackets: f[9], txErrs: f[10], txDrop: f[11],
		}
	}
	return out
}
