// Package profiling is the agent's profiling proxy, its one inbound HTTP surface.
// It mirrors the stock trace-agent's /profiling/v1/input handler. A profiler
// (dd-trace-go for Go programs, ddprof for C) POSTs a multipart pprof upload here,
// and the proxy forwards the body unchanged to the Datadog profiling intake,
// adding only the API key and the identifying headers the backend expects. The
// agent profiles nothing itself, exactly like the stock Agent.
package profiling

import (
	"bytes"
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"time"

	"github.com/0intro/dd-micro-agent/internal/intake"
)

// uploadPath is the endpoint dd-trace-go and ddprof upload to.
const uploadPath = "/profiling/v1/input"

// Options configures a Proxy.
type Options struct {
	ListenHost        string            // bind host, e.g. localhost
	ListenPort        int               // bind port, e.g. 8126
	NonLocal          bool              // bind 0.0.0.0 instead of ListenHost
	Endpoints         []intake.Endpoint // profiling intakes (main + additional), the first is primary
	Hostname          string
	Env               string // default_env reported to the intake
	Proxy             string // explicit https proxy URL, empty falls back to the environment
	SkipSSLValidation bool
	Logger            *slog.Logger
}

// maxRequestBytes bounds the upload the proxy buffers when fanning out to several
// intakes, matching the stock trace-agent's profiling_max_request_bytes default.
const maxRequestBytes = 50 << 20

// Proxy is an HTTP server that forwards profile uploads to the Datadog intake.
type Proxy struct {
	addr   string
	server *http.Server
	log    *slog.Logger
}

// New returns a Proxy. It fails if Options carries no endpoints or an endpoint
// URL does not parse.
func New(o Options) (*Proxy, error) {
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	h, err := newHandler(o)
	if err != nil {
		return nil, err
	}

	host := o.ListenHost
	if o.NonLocal {
		host = "0.0.0.0"
	}
	addr := net.JoinHostPort(host, strconv.Itoa(o.ListenPort))
	return &Proxy{
		addr:   addr,
		server: &http.Server{Addr: addr, Handler: h},
		log:    o.Logger,
	}, nil
}

// newHandler builds the routing mux: only POST /profiling/v1/input reaches the
// reverse proxy, everything else gets the mux's 404 or 405.
func newHandler(o Options) (http.Handler, error) {
	if len(o.Endpoints) == 0 {
		return nil, fmt.Errorf("profiling proxy has no endpoints configured")
	}
	targets := make([]*url.URL, len(o.Endpoints))
	keys := make([]string, len(o.Endpoints))
	for i, ep := range o.Endpoints {
		u, err := url.Parse(ep.URL)
		if err != nil {
			return nil, fmt.Errorf("profiling target %q: %w", ep.URL, err)
		}
		targets[i], keys[i] = u, ep.APIKey
	}
	rp := &httputil.ReverseProxy{
		Director:       director(o.Hostname, o.Env),
		ModifyResponse: logForwarded(o.Logger),
		ErrorHandler:   logFailed(o.Logger),
		Transport: &multiTransport{
			rt:       newTransport(o.Proxy, o.SkipSSLValidation),
			targets:  targets,
			keys:     keys,
			maxBytes: maxRequestBytes,
			log:      o.Logger,
		},
	}
	mux := http.NewServeMux()
	mux.Handle("POST "+uploadPath, rp)
	return mux, nil
}

// Run serves until ctx is cancelled. It returns a non-nil error only when the
// listener cannot start, so main can bring the agent down on a bind clash.
func (p *Proxy) Run(ctx context.Context) error {
	ln, err := net.Listen("tcp", p.addr)
	if err != nil {
		return err
	}
	p.log.Info("profiling proxy listening", "addr", p.addr)

	errc := make(chan error, 1)
	go func() { errc <- p.server.Serve(ln) }()

	select {
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		p.server.Shutdown(shutdown)
		return nil
	case err := <-errc:
		if err == http.ErrServerClosed {
			return nil
		}
		return err
	}
}

// director adds the headers the stock trace-agent adds that are the same for every
// intake: a Via marker and the host/env/version tags the backend stamps onto the
// profile. The per-target URL and DD-API-KEY are set by multiTransport, since they
// vary across orgs. The multipart body is left untouched.
func director(hostname, env string) func(*http.Request) {
	tags := fmt.Sprintf("host:%s,default_env:%s,agent_version:%s", hostname, env, intake.Version)
	return func(req *http.Request) {
		req.Header.Set("Via", "trace-agent "+intake.Version)
		req.Header.Set("X-Datadog-Additional-Tags", tags)
		if _, ok := req.Header["User-Agent"]; !ok {
			req.Header.Set("User-Agent", "") // suppress Go's default Go-http-client/1.1
		}
	}
}

// multiTransport forwards each upload to one or more profiling intakes. With a
// single target it streams the body straight through, the common no-additional
// case. With several it buffers the body once (failing an upload over maxBytes)
// and replays a clone to each target in order, returning the primary's response
// to the client and firing the rest off best-effort, mirroring the stock
// trace-agent's proxy.
type multiTransport struct {
	rt       http.RoundTripper
	targets  []*url.URL
	keys     []string
	maxBytes int64
	log      *slog.Logger
}

func (m *multiTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	setTarget := func(r *http.Request, u *url.URL, key string) {
		dst := *u
		r.URL = &dst
		r.Host = u.Host
		r.Header.Set("DD-API-KEY", key)
	}
	if len(m.targets) == 1 {
		setTarget(req, m.targets[0], m.keys[0])
		return m.rt.RoundTrip(req)
	}

	body, err := io.ReadAll(io.LimitReader(req.Body, m.maxBytes+1))
	req.Body.Close()
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > m.maxBytes {
		// An upload over the cap fails outright, since any truncation would
		// forward corrupt multipart to every intake.
		return nil, fmt.Errorf("profile upload exceeds %d bytes", m.maxBytes)
	}
	var resp *http.Response
	var rerr error
	for i, u := range m.targets {
		clone := req.Clone(req.Context())
		clone.Body = io.NopCloser(bytes.NewReader(body))
		setTarget(clone, u, m.keys[i])
		if i == 0 {
			resp, rerr = m.rt.RoundTrip(clone) // the primary's response goes to the client
			continue
		}
		if r, err := m.rt.RoundTrip(clone); err == nil {
			io.Copy(io.Discard, r.Body)
			r.Body.Close()
		} else {
			m.log.Warn("profile forward to additional endpoint failed", "url", u.String(), "err", err)
		}
	}
	return resp, rerr
}

// logForwarded notes each upload and replays the intake's 202 as 200, the
// backward-compatibility quirk the stock proxy keeps for older clients.
func logForwarded(log *slog.Logger) func(*http.Response) error {
	return func(resp *http.Response) error {
		if resp.StatusCode == http.StatusAccepted {
			resp.StatusCode = http.StatusOK
			resp.Status = http.StatusText(http.StatusOK)
		}
		log.Debug("profile forwarded", "status", resp.StatusCode)
		return nil
	}
}

func logFailed(log *slog.Logger) func(http.ResponseWriter, *http.Request, error) {
	return func(w http.ResponseWriter, _ *http.Request, err error) {
		log.Warn("profile forward failed", "err", err)
		w.WriteHeader(http.StatusBadGateway)
	}
}

func newTransport(proxyURL string, skipSSL bool) *http.Transport {
	t := &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   30 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		// The intake closes idle connections at 60s and the default profiler
		// period is also 60s, so an upload tends to race the close, and a
		// proxied POST has no GetBody for Go to retry with. Closing our side
		// first at 47s (the stock trace-agent's value, a prime that does not
		// divide 60) loses that race on purpose.
		IdleConnTimeout:     47 * time.Second,
		TLSHandshakeTimeout: 10 * time.Second,
	}
	if proxyURL != "" {
		if u, err := url.Parse(proxyURL); err == nil {
			t.Proxy = http.ProxyURL(u)
		}
	}
	if skipSSL {
		t.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
	}
	return t
}
