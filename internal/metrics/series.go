// Package metrics holds the metric data types and the DogStatsD aggregator.
// Series are the common currency: both the aggregator and the host collectors
// produce []Serie, and SendSeries ships them to the v1 series intake as JSON.
package metrics

import (
	"context"
	"encoding/json"
	"math"
	"strconv"
	"strings"

	"github.com/0intro/dd-micro-agent/internal/intake"
)

// SeriesSource produces series on demand. The host collectors implement it, and
// the aggregator pulls from each source at every flush so all metrics, pushed
// (DogStatsD) and pulled (host), ship in one request.
type SeriesSource interface {
	Collect() []Serie
}

// SerieType is the metric type as understood by the series API.
type SerieType string

const (
	TypeGauge SerieType = "gauge"
	TypeCount SerieType = "count"
	TypeRate  SerieType = "rate"
)

// Point is one (timestamp, value) sample. Timestamps are epoch seconds. It
// marshals as the two-element array [ts, value] the series API expects.
type Point struct {
	Ts    int64
	Value float64
}

// MarshalJSON renders the point as [ts, value].
func (p Point) MarshalJSON() ([]byte, error) {
	b := make([]byte, 0, 32)
	b = append(b, '[')
	b = strconv.AppendInt(b, p.Ts, 10)
	b = append(b, ',')
	b = strconv.AppendFloat(b, p.Value, 'g', -1, 64)
	b = append(b, ']')
	return b, nil
}

// Serie is one aggregated time series ready to ship.
type Serie struct {
	Name           string
	Points         []Point
	Type           SerieType
	Interval       int64
	Tags           []string
	Host           string
	Device         string // usually derived from a "device:" tag at marshal time
	SourceTypeName string // "System" for host series, like the stock Agent's checks
}

type serieJSON struct {
	Metric         string    `json:"metric"`
	Points         []Point   `json:"points"`
	Type           SerieType `json:"type"`
	Interval       int64     `json:"interval"`
	Tags           []string  `json:"tags"`
	Host           string    `json:"host"`
	Device         string    `json:"device,omitempty"`
	SourceTypeName string    `json:"source_type_name,omitempty"`
}

// MarshalSeries renders series as the {"series":[...]} payload. A "device:" tag
// is lifted into the top-level device field, which is how the backend keys disk
// and network metrics by device. A serie with a NaN or Inf point is skipped:
// encoding/json refuses non-finite floats, and one bad point must not void the
// whole payload (the stock Agent skips the bad item the same way).
func MarshalSeries(series []Serie) ([]byte, error) {
	payload := struct {
		Series []serieJSON `json:"series"`
	}{Series: make([]serieJSON, 0, len(series))}

	for _, s := range series {
		if !finitePoints(s.Points) {
			continue
		}
		tags, device := s.Tags, s.Device
		if device == "" {
			tags, device = liftDevice(tags)
		}
		if tags == nil {
			tags = []string{}
		}
		payload.Series = append(payload.Series, serieJSON{
			Metric:         s.Name,
			Points:         s.Points,
			Type:           s.Type,
			Interval:       s.Interval,
			Tags:           tags,
			Host:           s.Host,
			Device:         device,
			SourceTypeName: s.SourceTypeName,
		})
	}
	return json.Marshal(payload)
}

func finite(f float64) bool { return !math.IsNaN(f) && !math.IsInf(f, 0) }

func finitePoints(pts []Point) bool {
	for _, p := range pts {
		if !finite(p.Value) {
			return false
		}
	}
	return true
}

// maxSeriesPerPayload caps how many series go in one POST so a large flush stays
// well under the intake's payload-size limit. A few hundred KB per request.
const maxSeriesPerPayload = 1000

// SendSeries marshals series and posts them to every endpoint, splitting into
// several requests when there are many. The primary endpoint gates the returned
// error. An empty slice is a no-op.
func SendSeries(ctx context.Context, c *intake.Client, endpoints []intake.Endpoint, series []Serie) error {
	for start := 0; start < len(series); start += maxSeriesPerPayload {
		end := min(start+maxSeriesPerPayload, len(series))
		body, err := MarshalSeries(series[start:end])
		if err != nil {
			return err
		}
		if _, err := c.PostAll(ctx, endpoints, body); err != nil {
			return err
		}
	}
	return nil
}

// liftDevice removes "device:" tags and returns the first device value found.
// It allocates only when a device tag is present.
func liftDevice(tags []string) ([]string, string) {
	has := false
	for _, t := range tags {
		if strings.HasPrefix(t, "device:") {
			has = true
			break
		}
	}
	if !has {
		return tags, ""
	}
	clean := make([]string, 0, len(tags))
	var device string
	for _, t := range tags {
		if dev, ok := strings.CutPrefix(t, "device:"); ok {
			if device == "" {
				device = dev
			}
			continue
		}
		clean = append(clean, t)
	}
	return clean, device
}
