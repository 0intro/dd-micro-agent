package process

// Transport to the Datadog process intake. Unlike internal/intake (which gzips
// JSON), the process intake takes an uncompressed protobuf body with its own
// header set, and, because realtime is response-driven, we need the response
// body back. So this package owns its submission, the same way metrics and logs
// own theirs. The retry/backoff shape mirrors internal/intake.

import (
	"bytes"
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/0intro/dd-micro-agent/internal/intake"
)

const backoffMax = 30 * time.Second

// submitter POSTs framed process messages and returns the intake's response body.
// It carries no key or URL: those ride with the Endpoint each post targets, so one
// submitter fans a payload out to several orgs (dual-shipping).
type submitter struct {
	http     *http.Client
	hostname string
	maxTries int
	backoff  time.Duration
	log      *slog.Logger
}

func newSubmitter(o Options) *submitter {
	transport := &http.Transport{Proxy: http.ProxyFromEnvironment}
	if o.Proxy != "" {
		if u, err := url.Parse(o.Proxy); err == nil {
			transport.Proxy = http.ProxyURL(u)
		} else {
			// Going direct in a proxied network fails with opaque dial errors,
			// so name the discarded setting.
			o.Logger.Warn("ignoring unparsable proxy URL, falling back to the environment proxy", "proxy", o.Proxy, "err", err)
		}
	}
	if o.SkipSSLValidation {
		transport.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
	}
	return &submitter{
		http:     &http.Client{Timeout: 30 * time.Second, Transport: transport},
		hostname: o.Hostname,
		maxTries: 4,
		backoff:  time.Second,
		log:      o.Logger,
	}
}

// post sends one framed message to ep, retrying transient failures. On success it
// returns the (uncompressed protobuf) response body, which the caller decodes for
// the realtime toggle.
func (s *submitter) post(ctx context.Context, ep intake.Endpoint, framed []byte) ([]byte, error) {
	wait := s.backoff
	var status int
	var err error
	for try := 0; try < s.maxTries; try++ {
		if try > 0 {
			if err := sleep(ctx, wait); err != nil {
				return nil, err
			}
			if wait *= 2; wait > backoffMax {
				wait = backoffMax
			}
		}
		var body []byte
		status, body, err = s.do(ctx, ep, framed)
		switch {
		case err == nil:
			return body, nil
		case permanent(status):
			return nil, err
		case try+1 < s.maxTries:
			s.log.Debug("process post failed, will retry", "status", status, "err", err, "try", try+1)
		}
	}
	return nil, fmt.Errorf("post %s: giving up after %d tries: %w", ep.URL, s.maxTries, err)
}

func (s *submitter) do(ctx context.Context, ep intake.Endpoint, framed []byte) (int, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, ep.URL, bytes.NewReader(framed))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("DD-Api-Key", ep.APIKey)
	req.Header.Set("Content-Type", "application/x-protobuf")
	req.Header.Set("X-Dd-Hostname", s.hostname)
	req.Header.Set("X-Dd-Processagentversion", intake.Version)
	req.Header.Set("X-DD-Agent-Timestamp", strconv.FormatInt(time.Now().Unix(), 10))
	req.Header.Set("User-Agent", "dd-micro-agent/"+intake.Version)

	resp, err := s.http.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	if resp.StatusCode/100 == 2 {
		return resp.StatusCode, body, nil
	}
	return resp.StatusCode, nil, fmt.Errorf("unexpected status %s", resp.Status)
}

// permanent reports whether a status should not be retried.
func permanent(status int) bool {
	switch status {
	case http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden, http.StatusRequestEntityTooLarge:
		return true
	}
	return false
}

func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
