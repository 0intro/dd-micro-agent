package metrics

// SampleType is the metric type parsed from a DogStatsD line.
type SampleType uint8

const (
	SampleGauge        SampleType = iota // g
	SampleCounter                        // c
	SampleHistogram                      // h
	SampleTiming                         // ms
	SampleDistribution                   // d
	SampleSet                            // s
)

// Sample is one parsed DogStatsD submission. SampleRate defaults to 1. For a
// counter a rate of, say, 0.1 means the client sent one in ten, so the value is
// scaled up by 10 on aggregation.
type Sample struct {
	Name       string
	Value      float64
	Str        string // set member (raw value text), used only for SampleSet
	Type       SampleType
	Tags       []string
	SampleRate float64
}
