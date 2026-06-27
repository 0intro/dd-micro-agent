// Package intake is the transport to the Datadog backend. It knows how to POST a
// JSON body (gzip it, attach the Datadog headers, and retry transient failures)
// but nothing about what the body contains. The metrics and logs packages own
// their own payload shapes and call Post with the right Endpoint.
//
// The Client is a keyless shared transport: the API key travels with each
// Endpoint, not the Client, so one Client fans a payload out to several Datadog
// orgs (the stock Agent's additional_endpoints dual-shipping), each authenticated
// with its own key. PostAll and PostAllFunc do that fan-out: the first endpoint is
// the primary and gates success, the rest are best-effort.
package intake

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"sync"
	"time"
)

// Version is reported in the User-Agent and DD-Agent-Version headers.
const Version = "0.1.0"

// Client posts compressed JSON payloads to Datadog intake endpoints. It holds no
// API key: the key rides with each Endpoint, so one Client serves many orgs. A
// Client is safe for concurrent use by multiple goroutines.
type Client struct {
	http     *http.Client
	maxTries int
	backoff  time.Duration // initial backoff, doubles each retry up to backoffMax
	log      *slog.Logger
}

const backoffMax = 30 * time.Second

// additionalTimeout caps how long a single best-effort (non-primary) endpoint may
// take. The primary runs on the caller's context with the full retry ladder, but a
// dead secondary must not stall the synchronous metrics flush or the bounded
// shutdown drain, so it gets this short deadline instead of the whole ~7s of retries.
const additionalTimeout = 5 * time.Second

// Endpoint is one delivery destination: a full intake URL and the API key that
// authenticates to it. Reliable controls only how loudly a failed best-effort
// delivery is logged (the stock logs is_reliable flag), it does not gate success.
type Endpoint struct {
	URL      string
	APIKey   string
	Reliable bool
}

// Options configures a Client. The zero value is usable.
type Options struct {
	SkipSSLValidation bool
	Proxy             string // explicit https proxy URL. Empty falls back to the environment
	Timeout           time.Duration
	MaxTries          int
	Backoff           time.Duration
	Logger            *slog.Logger
}

// New returns a Client. Sensible defaults fill in any zero Options field.
func New(opts Options) *Client {
	if opts.Timeout == 0 {
		opts.Timeout = 20 * time.Second
	}
	if opts.MaxTries == 0 {
		opts.MaxTries = 4
	}
	if opts.Backoff == 0 {
		opts.Backoff = time.Second
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}

	transport := &http.Transport{Proxy: http.ProxyFromEnvironment}
	if opts.Proxy != "" {
		if u, err := url.Parse(opts.Proxy); err == nil {
			transport.Proxy = http.ProxyURL(u)
		} else {
			// Going direct in a proxied network fails with opaque dial errors,
			// so name the discarded setting.
			opts.Logger.Warn("ignoring unparsable proxy URL, falling back to the environment proxy", "proxy", opts.Proxy, "err", err)
		}
	}
	if opts.SkipSSLValidation {
		transport.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
	}

	return &Client{
		http:     &http.Client{Timeout: opts.Timeout, Transport: transport},
		maxTries: opts.MaxTries,
		backoff:  opts.Backoff,
		log:      opts.Logger,
	}
}

// Post gzips body and POSTs it as application/json to ep.URL, authenticating with
// ep.APIKey and retrying transient failures with exponential backoff. It returns
// the last HTTP status code seen (0 if no response was obtained). A nil error means
// the payload was accepted.
func (c *Client) Post(ctx context.Context, ep Endpoint, body []byte) (int, error) {
	gz, err := compress(body)
	if err != nil {
		return 0, err
	}
	return c.send(ctx, ep, gz)
}

// send posts already-gzipped bytes to ep with the retry ladder. It is the shared
// tail of Post and the fan-out methods, so PostAll can compress one body once and
// hand the same bytes to every endpoint rather than gzip per endpoint.
func (c *Client) send(ctx context.Context, ep Endpoint, gz []byte) (int, error) {
	// A URL that cannot form a request can never succeed, so it fails before
	// the retry ladder.
	if _, err := http.NewRequestWithContext(ctx, http.MethodPost, ep.URL, nil); err != nil {
		return 0, err
	}

	wait := c.backoff
	var status int
	var err error
	for try := 0; try < c.maxTries; try++ {
		if try > 0 {
			if err := sleep(ctx, wait); err != nil {
				return status, err
			}
			if wait *= 2; wait > backoffMax {
				wait = backoffMax
			}
		}

		status, err = c.do(ctx, ep, gz)
		switch {
		case err == nil:
			return status, nil
		case Permanent(status):
			return status, err
		case try+1 < c.maxTries:
			c.log.Debug("intake post failed, will retry", "url", ep.URL, "status", status, "err", err, "try", try+1)
		}
	}
	return status, fmt.Errorf("post %s: giving up after %d tries: %w", ep.URL, c.maxTries, err)
}

// PostAllFunc delivers a payload to every endpoint, building the body per endpoint.
// build is called with each endpoint's API key, which the v5 /intake/ envelope (for
// events and host metadata) carries in the body, so each org gets its own.
//
// endpoints[0] is the primary: it is built and posted on ctx with the full retry
// ladder, and its (status, error) is returned so the caller can gate a delivery
// decision (a log offset advance, say) on it. endpoints[1:] are best-effort: each
// is posted under a short deadline, and any error there, whether from build or post,
// is logged and swallowed, so one org being down never blocks the others or the
// primary. An empty list is a no-op.
func (c *Client) PostAllFunc(ctx context.Context, endpoints []Endpoint, build func(apiKey string) ([]byte, error)) (int, error) {
	if len(endpoints) == 0 {
		return 0, nil
	}
	primary := endpoints[0]
	body, err := build(primary.APIKey)
	if err != nil {
		return 0, err
	}
	status, err := c.Post(ctx, primary, body)
	for _, ep := range endpoints[1:] {
		body, err := build(ep.APIKey)
		if err != nil {
			c.logAdditional(ep, "additional endpoint payload build failed", err)
			continue
		}
		gz, err := compress(body)
		if err != nil {
			c.logAdditional(ep, "additional endpoint gzip failed", err)
			continue
		}
		c.postAdditional(ctx, ep, gz)
	}
	return status, err
}

// PostAll delivers the same body to every endpoint. Because the body does not vary
// by org (series, checks, logs, inventory), it gzips once and hands the same bytes
// to every endpoint, so the compression cost is paid per payload, not per endpoint.
func (c *Client) PostAll(ctx context.Context, endpoints []Endpoint, body []byte) (int, error) {
	if len(endpoints) == 0 {
		return 0, nil
	}
	gz, err := compress(body)
	if err != nil {
		return 0, err
	}
	status, err := c.send(ctx, endpoints[0], gz)
	for _, ep := range endpoints[1:] {
		c.postAdditional(ctx, ep, gz)
	}
	return status, err
}

// postAdditional delivers already-gzipped bytes to one best-effort endpoint under a
// short deadline. A reliable endpoint's failure is worth a warning, an unreliable
// one's only a debug line, but neither is returned: the primary alone decides success.
func (c *Client) postAdditional(ctx context.Context, ep Endpoint, gz []byte) {
	ctx, cancel := context.WithTimeout(ctx, additionalTimeout)
	defer cancel()
	if _, err := c.send(ctx, ep, gz); err != nil {
		c.logAdditional(ep, "additional endpoint post failed", err)
	}
}

func (c *Client) logAdditional(ep Endpoint, msg string, err error) {
	if ep.Reliable {
		c.log.Warn(msg, "url", ep.URL, "err", err)
	} else {
		c.log.Debug(msg, "url", ep.URL, "err", err)
	}
}

// do performs one request and reports a non-nil error for any non-2xx response.
func (c *Client) do(ctx context.Context, ep Endpoint, gz []byte) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, ep.URL, bytes.NewReader(gz))
	if err != nil {
		return 0, err
	}
	req.Header.Set("DD-API-KEY", ep.APIKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Content-Encoding", "gzip")
	req.Header.Set("DD-Agent-Version", Version)
	req.Header.Set("User-Agent", "dd-micro-agent/"+Version)

	resp, err := c.http.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16)) // drain to reuse the connection

	if resp.StatusCode/100 == 2 {
		return resp.StatusCode, nil
	}
	return resp.StatusCode, fmt.Errorf("unexpected status %s", resp.Status)
}

// Permanent reports whether a status means the request should not be retried:
// malformed request, bad/forbidden key, or payload too large (the caller must
// shrink or drop the payload instead).
func Permanent(status int) bool {
	switch status {
	case http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden, http.StatusRequestEntityTooLarge:
		return true
	}
	return false
}

// gzipWriters reuses deflate compressors across posts. gzip.NewWriter lazily
// allocates about a megabyte of window and hash-table state on its first write, and
// one Client fans a payload out to several endpoints from several goroutines, so a
// fresh writer per post would churn that megabyte on every flush. A pooled writer is
// Reset onto each new buffer, so only the output bytes are allocated. sync.Pool
// drops its entries under GC pressure, so an idle agent does not retain them.
var gzipWriters = sync.Pool{New: func() any { return gzip.NewWriter(io.Discard) }}

func compress(body []byte) ([]byte, error) {
	var buf bytes.Buffer
	w := gzipWriters.Get().(*gzip.Writer)
	w.Reset(&buf)
	defer gzipWriters.Put(w)
	if _, err := w.Write(body); err != nil {
		return nil, err
	}
	if err := w.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
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
