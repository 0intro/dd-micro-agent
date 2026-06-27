package profiling

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"mime/multipart"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"testing"

	"github.com/0intro/dd-micro-agent/internal/intake"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// multipartUpload builds a profiling upload like dd-trace-go/ddprof send: an
// event.json part plus one pprof attachment. It returns the body and its content
// type so the test can post it and later assert byte-for-byte passthrough.
func multipartUpload(t *testing.T) (body []byte, contentType string) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	ev, _ := mw.CreateFormFile("event", "event.json")
	ev.Write([]byte(`{"version":"4","family":"go","attachments":["cpu.pprof"]}`))
	pp, _ := mw.CreateFormFile("cpu.pprof", "cpu.pprof")
	pp.Write([]byte("not really pprof, just bytes"))
	mw.Close()
	return buf.Bytes(), mw.FormDataContentType()
}

func TestProxyForwardsToIntake(t *testing.T) {
	type seen struct {
		path, key, via, tags, ua string
		body                     []byte
	}
	got := make(chan seen, 1)
	intakeSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got <- seen{
			path: r.URL.Path,
			key:  r.Header.Get("DD-API-KEY"),
			via:  r.Header.Get("Via"),
			tags: r.Header.Get("X-Datadog-Additional-Tags"),
			ua:   r.Header.Get("User-Agent"),
			body: b,
		}
		w.WriteHeader(http.StatusAccepted) // intake answers 202, proxy must replay 200
	}))
	defer intakeSrv.Close()

	h, err := newHandler(Options{
		Endpoints: []intake.Endpoint{{URL: intakeSrv.URL + "/api/v2/profile", APIKey: "secret"}},
		Hostname:  "host1", Env: "prod", Logger: discardLogger(),
	})
	if err != nil {
		t.Fatal(err)
	}
	front := httptest.NewServer(h)
	defer front.Close()

	body, ct := multipartUpload(t)
	req, _ := http.NewRequest(http.MethodPost, front.URL+uploadPath, bytes.NewReader(body))
	req.Header.Set("Content-Type", ct)
	req.Header.Set("User-Agent", "") // a client that sends none, like the tracers
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("client saw status %d, want 200 (202 must be rewritten)", resp.StatusCode)
	}

	s := <-got
	if s.path != "/api/v2/profile" {
		t.Errorf("intake path = %q, want /api/v2/profile", s.path)
	}
	if s.key != "secret" {
		t.Errorf("DD-API-KEY = %q, want secret", s.key)
	}
	if want := "trace-agent " + intake.Version; s.via != want {
		t.Errorf("Via = %q, want %q", s.via, want)
	}
	if want := "host:host1,default_env:prod,agent_version:" + intake.Version; s.tags != want {
		t.Errorf("X-Datadog-Additional-Tags = %q, want %q", s.tags, want)
	}
	if s.ua != "" {
		t.Errorf("User-Agent = %q, want empty", s.ua)
	}
	if !bytes.Equal(s.body, body) {
		t.Errorf("forwarded body changed: got %d bytes, sent %d", len(s.body), len(body))
	}
}

// TestProxyFansOutToMultipleEndpoints proves the multiTransport replays one upload
// to every configured intake, each with its own API key and the body unchanged.
func TestProxyFansOutToMultipleEndpoints(t *testing.T) {
	type seen struct {
		key  string
		body []byte
	}
	mk := func(ch chan seen) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			b, _ := io.ReadAll(r.Body)
			ch <- seen{key: r.Header.Get("DD-API-KEY"), body: b}
			w.WriteHeader(http.StatusAccepted)
		}
	}
	gotA, gotB := make(chan seen, 1), make(chan seen, 1)
	srvA := httptest.NewServer(mk(gotA))
	srvB := httptest.NewServer(mk(gotB))
	defer srvA.Close()
	defer srvB.Close()

	h, err := newHandler(Options{
		Endpoints: []intake.Endpoint{
			{URL: srvA.URL + "/api/v2/profile", APIKey: "KEY_A"},
			{URL: srvB.URL + "/api/v2/profile", APIKey: "KEY_B"},
		},
		Hostname: "host1", Env: "prod", Logger: discardLogger(),
	})
	if err != nil {
		t.Fatal(err)
	}
	front := httptest.NewServer(h)
	defer front.Close()

	body, ct := multipartUpload(t)
	req, _ := http.NewRequest(http.MethodPost, front.URL+uploadPath, bytes.NewReader(body))
	req.Header.Set("Content-Type", ct)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("client saw status %d, want 200", resp.StatusCode)
	}

	a, b := <-gotA, <-gotB
	if a.key != "KEY_A" || b.key != "KEY_B" {
		t.Errorf("keys = %q / %q, want KEY_A / KEY_B", a.key, b.key)
	}
	if !bytes.Equal(a.body, body) || !bytes.Equal(b.body, body) {
		t.Errorf("forwarded body changed (a=%d b=%d, sent %d)", len(a.body), len(b.body), len(body))
	}
}

// TestProxyRejectsOversizedUpload proves the fan-out buffering path fails a body
// over the cap with a 502 rather than truncating it and forwarding corrupt
// multipart to the intakes.
func TestProxyRejectsOversizedUpload(t *testing.T) {
	forwarded := make(chan struct{}, 2)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		forwarded <- struct{}{}
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()

	target, err := url.Parse(srv.URL + "/api/v2/profile")
	if err != nil {
		t.Fatal(err)
	}
	const limit = 1 << 10 // a small cap so the test body stays small
	rp := &httputil.ReverseProxy{
		Director:     director("host1", "prod"),
		ErrorHandler: logFailed(discardLogger()),
		Transport: &multiTransport{
			rt:       http.DefaultTransport,
			targets:  []*url.URL{target, target},
			keys:     []string{"KEY_A", "KEY_B"},
			maxBytes: limit,
			log:      discardLogger(),
		},
	}
	front := httptest.NewServer(rp)
	defer front.Close()

	body := bytes.Repeat([]byte("x"), limit+1)
	resp, err := http.Post(front.URL+uploadPath, "application/octet-stream", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Errorf("client saw status %d, want 502 for an oversized upload", resp.StatusCode)
	}
	select {
	case <-forwarded:
		t.Error("oversized upload was forwarded to an intake")
	default:
	}
}

func TestProxyRejectsOtherRequests(t *testing.T) {
	h, err := newHandler(Options{Endpoints: []intake.Endpoint{{URL: "http://intake.invalid/api/v2/profile"}}, Logger: discardLogger()})
	if err != nil {
		t.Fatal(err)
	}
	front := httptest.NewServer(h)
	defer front.Close()

	cases := []struct {
		method, path string
		want         int
	}{
		{http.MethodGet, uploadPath, http.StatusMethodNotAllowed},
		{http.MethodPost, "/v0.4/traces", http.StatusNotFound},
		{http.MethodGet, "/", http.StatusNotFound},
	}
	for _, c := range cases {
		req, _ := http.NewRequest(c.method, front.URL+c.path, nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v", c.method, c.path, err)
		}
		resp.Body.Close()
		if resp.StatusCode != c.want {
			t.Errorf("%s %s: status %d, want %d", c.method, c.path, resp.StatusCode, c.want)
		}
	}
}

func TestRunReportsBindFailure(t *testing.T) {
	busy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer busy.Close()
	port := busy.Addr().(*net.TCPAddr).Port

	p, err := New(Options{
		ListenHost: "127.0.0.1", ListenPort: port,
		Endpoints: []intake.Endpoint{{URL: "http://intake.invalid/api/v2/profile"}}, Logger: discardLogger(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Run(context.Background()); err == nil {
		t.Errorf("Run on busy port %d returned nil, want a bind error", port)
	}
}
