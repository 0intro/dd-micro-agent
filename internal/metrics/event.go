package metrics

import (
	"context"
	"encoding/json"

	"github.com/0intro/dd-micro-agent/internal/intake"
)

// Event is one event. Events ship in the legacy v5 /intake/ envelope (the same endpoint
// and gzipped transport as host metadata), grouped by source type. This is how the stock
// Agent submits them (serializer SubmitV1Intake -> Events). The public /api/v1/events
// endpoint is not used: it rejects gzipped bodies, which our intake client always sends.
// The JSON field names below are the intake's (msg_title/msg_text/timestamp).
type Event struct {
	Title          string   `json:"msg_title"`
	Text           string   `json:"msg_text"`
	Timestamp      int64    `json:"timestamp,omitempty"` // epoch seconds
	Priority       string   `json:"priority,omitempty"`  // "normal" (default) or "low"
	AlertType      string   `json:"alert_type,omitempty"`
	Host           string   `json:"host,omitempty"`
	Tags           []string `json:"tags,omitempty"`
	AggregationKey string   `json:"aggregation_key,omitempty"`
	SourceTypeName string   `json:"source_type_name,omitempty"`
}

// MarshalEvents builds the /intake/ events payload: {apiKey, events grouped by source
// type, internalHostname}. Events with no source type group under "api".
func MarshalEvents(events []Event, apiKey, hostname string) ([]byte, error) {
	grouped := make(map[string][]Event)
	for _, e := range events {
		st := e.SourceTypeName
		if st == "" {
			st = "api"
		}
		grouped[st] = append(grouped[st], e)
	}
	return json.Marshal(struct {
		APIKey           string             `json:"apiKey"`
		Events           map[string][]Event `json:"events"`
		InternalHostname string             `json:"internalHostname"`
	}{APIKey: apiKey, Events: grouped, InternalHostname: hostname})
}

// SendEvents ships all events in one /intake/ envelope to every endpoint. The
// envelope embeds the API key, so each org gets a body built with its own key
// (PostAllFunc). An empty slice is a no-op.
func SendEvents(ctx context.Context, c *intake.Client, endpoints []intake.Endpoint, hostname string, events []Event) error {
	if len(events) == 0 {
		return nil
	}
	_, err := c.PostAllFunc(ctx, endpoints, func(apiKey string) ([]byte, error) {
		return MarshalEvents(events, apiKey, hostname)
	})
	return err
}
