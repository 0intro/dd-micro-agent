package metrics

import (
	"context"
	"encoding/json"

	"github.com/0intro/dd-micro-agent/internal/intake"
)

// ServiceCheckStatus is a Datadog service-check status.
type ServiceCheckStatus int

const (
	ServiceOK       ServiceCheckStatus = 0
	ServiceWarning  ServiceCheckStatus = 1
	ServiceCritical ServiceCheckStatus = 2
	ServiceUnknown  ServiceCheckStatus = 3
)

// ServiceCheck is one service check. The field names match the check_run intake
// (pkg/metrics/servicecheck), which takes a JSON array of these.
type ServiceCheck struct {
	Check     string             `json:"check"`
	Host      string             `json:"host_name"`
	Timestamp int64              `json:"timestamp"`
	Status    ServiceCheckStatus `json:"status"`
	Message   string             `json:"message,omitempty"`
	Tags      []string           `json:"tags"`
}

// SendServiceChecks marshals checks as a JSON array and posts them to every
// endpoint (the /api/v1/check_run intake). An empty slice is a no-op.
func SendServiceChecks(ctx context.Context, c *intake.Client, endpoints []intake.Endpoint, checks []ServiceCheck) error {
	if len(checks) == 0 {
		return nil
	}
	for i := range checks {
		if checks[i].Tags == nil {
			checks[i].Tags = []string{} // [] for tidiness. The intake also accepts the stock Agent's null
		}
	}
	body, err := json.Marshal(checks)
	if err != nil {
		return err
	}
	_, err = c.PostAll(ctx, endpoints, body)
	return err
}
