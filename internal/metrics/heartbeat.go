package metrics

import (
	"time"

	"github.com/0intro/dd-micro-agent/internal/intake"
)

// HeartbeatSource emits datadog.agent.running=1 once per flush, tagged with the
// agent version like the stock Agent's heartbeat. The micro-agent produces no
// other self-telemetry, so this gives the stock "agent is running" / host-down
// monitors a metric to watch. It implements SeriesSource and is wired into the
// aggregator's Sources by main, so the aggregator fills in host, interval, and
// the global tags like any other source.
type HeartbeatSource struct{}

// Collect returns the single heartbeat serie (timestamped now).
func (HeartbeatSource) Collect() []Serie {
	return []Serie{{
		Name:   "datadog.agent.running",
		Type:   TypeGauge,
		Points: []Point{{Ts: time.Now().Unix(), Value: 1}},
		Tags:   []string{"version:" + intake.Version},
	}}
}
