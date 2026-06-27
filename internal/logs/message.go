// Package logs tails files and ships their lines to the logs intake. A Message
// is one log line plus its source metadata. A batch of Messages marshals to the
// JSON array the v2 logs intake expects.
package logs

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/0intro/dd-micro-agent/internal/intake"
)

// Message is a single log line with the metadata of the source it came from.
type Message struct {
	Content   []byte
	Status    string    // defaults to "info"
	Timestamp time.Time // defaults to now at render time
	Service   string
	Source    string   // becomes ddsource
	Tags      []string // becomes the comma-joined ddtags

	// Registry bookkeeping, never serialized. sourceID identifies the file and
	// offset is the byte position just past this line, so a successful send can
	// advance the registry to a clean newline boundary. A journald message carries
	// its opaque cursor instead of an offset (see journald_linux.go), and the
	// batcher advances by whichever is set.
	sourceID string
	offset   int64
	cursor   string
}

type jsonLog struct {
	Message   string `json:"message"`
	Status    string `json:"status"`
	Timestamp int64  `json:"timestamp"` // epoch milliseconds
	Hostname  string `json:"hostname"`
	Service   string `json:"service"`
	Source    string `json:"ddsource"`
	Tags      string `json:"ddtags"`
}

// MarshalLogs renders messages as the JSON array the logs intake accepts.
func MarshalLogs(msgs []Message, hostname string) ([]byte, error) {
	arr := make([]jsonLog, len(msgs))
	for i, m := range msgs {
		ts := m.Timestamp
		if ts.IsZero() {
			ts = time.Now()
		}
		status := m.Status
		if status == "" {
			status = "info"
		}
		arr[i] = jsonLog{
			Message:   string(m.Content),
			Status:    status,
			Timestamp: ts.UnixMilli(),
			Hostname:  hostname,
			Service:   m.Service,
			Source:    m.Source,
			Tags:      strings.Join(m.Tags, ","),
		}
	}
	return json.Marshal(arr)
}

// SendLogs marshals messages and posts them to every endpoint. The primary
// endpoint gates the returned status and error, which the batcher uses to decide
// whether to advance the registry, retry, or drop. An empty batch is a no-op.
func SendLogs(ctx context.Context, c *intake.Client, endpoints []intake.Endpoint, hostname string, msgs []Message) (int, error) {
	if len(msgs) == 0 {
		return 0, nil
	}
	body, err := MarshalLogs(msgs, hostname)
	if err != nil {
		return 0, err
	}
	return c.PostAll(ctx, endpoints, body)
}
