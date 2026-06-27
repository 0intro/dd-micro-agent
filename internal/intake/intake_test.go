package intake

import (
	"compress/gzip"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestPostSendsGzippedJSONWithHeaders(t *testing.T) {
	var (
		key, enc, ctype string
		body            []byte
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key, enc, ctype = r.Header.Get("DD-API-KEY"), r.Header.Get("Content-Encoding"), r.Header.Get("Content-Type")
		gr, err := gzip.NewReader(r.Body)
		if err != nil {
			t.Errorf("body not gzip: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		body, _ = io.ReadAll(gr)
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()

	c := New(Options{})
	status, err := c.Post(context.Background(), Endpoint{URL: srv.URL, APIKey: "secret"}, []byte(`{"hello":"world"}`))
	if err != nil {
		t.Fatalf("Post: %v", err)
	}
	if status != http.StatusAccepted {
		t.Errorf("status = %d, want 202", status)
	}
	if key != "secret" || enc != "gzip" || ctype != "application/json" {
		t.Errorf("headers: key=%q enc=%q ctype=%q", key, enc, ctype)
	}
	if string(body) != `{"hello":"world"}` {
		t.Errorf("decoded body = %q", body)
	}
}

func TestPostRetriesThenSucceeds(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if atomic.AddInt32(&calls, 1) < 3 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := New(Options{Backoff: time.Millisecond, MaxTries: 5})
	status, err := c.Post(context.Background(), Endpoint{URL: srv.URL, APIKey: "k"}, []byte("{}"))
	if err != nil {
		t.Fatalf("Post: %v", err)
	}
	if status != http.StatusOK {
		t.Errorf("status = %d, want 200", status)
	}
	if got := atomic.LoadInt32(&calls); got != 3 {
		t.Errorf("calls = %d, want 3", got)
	}
}

func TestPostPermanentStatusIsNotRetried(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer srv.Close()

	c := New(Options{Backoff: time.Millisecond, MaxTries: 5})
	status, err := c.Post(context.Background(), Endpoint{URL: srv.URL, APIKey: "k"}, []byte("{}"))
	if err == nil {
		t.Fatal("Post should fail on 400")
	}
	if status != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", status)
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Errorf("calls = %d, want 1 (no retry on permanent)", got)
	}
}

func TestPostGivesUpAfterMaxTries(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer srv.Close()

	c := New(Options{Backoff: time.Millisecond, MaxTries: 3})
	_, err := c.Post(context.Background(), Endpoint{URL: srv.URL, APIKey: "k"}, []byte("{}"))
	if err == nil {
		t.Fatal("Post should fail after exhausting retries")
	}
	if got := atomic.LoadInt32(&calls); got != 3 {
		t.Errorf("calls = %d, want 3", got)
	}
}

// keySink records, per server, the API key and body of the last request it saw.
type keySink struct {
	mu   sync.Mutex
	key  string
	body string
	hits int
}

func (s *keySink) handler(status int) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		gr, err := gzip.NewReader(r.Body)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		body, _ := io.ReadAll(gr)
		s.mu.Lock()
		s.key, s.body, s.hits = r.Header.Get("DD-API-KEY"), string(body), s.hits+1
		s.mu.Unlock()
		w.WriteHeader(status)
	}
}

func (s *keySink) get() (key, body string, hits int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.key, s.body, s.hits
}

// TestPostAllFansOutWithPerEndpointKey is the core dual-shipping assertion: the same
// body reaches every endpoint, each authenticated with its own key.
func TestPostAllFansOutWithPerEndpointKey(t *testing.T) {
	var a, b keySink
	sa := httptest.NewServer(a.handler(http.StatusAccepted))
	sb := httptest.NewServer(b.handler(http.StatusAccepted))
	defer sa.Close()
	defer sb.Close()

	c := New(Options{Backoff: time.Millisecond})
	eps := []Endpoint{{URL: sa.URL, APIKey: "KEY_A"}, {URL: sb.URL, APIKey: "KEY_B", Reliable: true}}
	status, err := c.PostAll(context.Background(), eps, []byte(`{"v":1}`))
	if err != nil {
		t.Fatalf("PostAll: %v", err)
	}
	if status != http.StatusAccepted {
		t.Errorf("status = %d, want 202 (primary)", status)
	}
	if k, body, hits := a.get(); k != "KEY_A" || body != `{"v":1}` || hits != 1 {
		t.Errorf("primary saw key=%q body=%q hits=%d", k, body, hits)
	}
	if k, body, hits := b.get(); k != "KEY_B" || body != `{"v":1}` || hits != 1 {
		t.Errorf("additional saw key=%q body=%q hits=%d", k, body, hits)
	}
}

// TestPostAllAdditionalFailureIsSwallowed: a down secondary must not fail the call.
func TestPostAllAdditionalFailureIsSwallowed(t *testing.T) {
	var a keySink
	sa := httptest.NewServer(a.handler(http.StatusAccepted))
	sb := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer sa.Close()
	defer sb.Close()

	c := New(Options{Backoff: time.Millisecond, MaxTries: 2})
	eps := []Endpoint{{URL: sa.URL, APIKey: "KEY_A"}, {URL: sb.URL, APIKey: "KEY_B"}}
	if _, err := c.PostAll(context.Background(), eps, []byte("{}")); err != nil {
		t.Fatalf("PostAll should ignore an additional-endpoint failure, got %v", err)
	}
	if _, _, hits := a.get(); hits != 1 {
		t.Errorf("primary hits = %d, want 1", hits)
	}
}

// TestPostAllPrimaryFailureReturnsError: the primary alone gates success.
func TestPostAllPrimaryFailureReturnsError(t *testing.T) {
	sa := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
	}))
	var b keySink
	sb := httptest.NewServer(b.handler(http.StatusAccepted))
	defer sa.Close()
	defer sb.Close()

	c := New(Options{Backoff: time.Millisecond})
	eps := []Endpoint{{URL: sa.URL, APIKey: "KEY_A"}, {URL: sb.URL, APIKey: "KEY_B"}}
	status, err := c.PostAll(context.Background(), eps, []byte("{}"))
	if err == nil {
		t.Fatal("PostAll should surface the primary's failure")
	}
	if status != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", status)
	}
	// The additional endpoint is still best-effort delivered even when the primary failed.
	if _, _, hits := b.get(); hits != 1 {
		t.Errorf("additional hits = %d, want 1", hits)
	}
}

// TestPostAllFuncPerEndpointBody proves the body is rebuilt per org, so the API key
// the v5 /intake/ envelope embeds matches the org it is shipped to.
func TestPostAllFuncPerEndpointBody(t *testing.T) {
	var a, b keySink
	sa := httptest.NewServer(a.handler(http.StatusAccepted))
	sb := httptest.NewServer(b.handler(http.StatusAccepted))
	defer sa.Close()
	defer sb.Close()

	c := New(Options{Backoff: time.Millisecond})
	eps := []Endpoint{{URL: sa.URL, APIKey: "KEY_A"}, {URL: sb.URL, APIKey: "KEY_B", Reliable: true}}
	build := func(apiKey string) ([]byte, error) { return []byte(`{"apiKey":"` + apiKey + `"}`), nil }
	if _, err := c.PostAllFunc(context.Background(), eps, build); err != nil {
		t.Fatalf("PostAllFunc: %v", err)
	}
	if _, body, _ := a.get(); body != `{"apiKey":"KEY_A"}` {
		t.Errorf("primary body = %q, want key A embedded", body)
	}
	if _, body, _ := b.get(); body != `{"apiKey":"KEY_B"}` {
		t.Errorf("additional body = %q, want key B embedded", body)
	}
}

// TestPostAllFuncBuildErrorSplit: a primary build error is returned, an additional
// build error is swallowed.
func TestPostAllFuncBuildErrorSplit(t *testing.T) {
	var a keySink
	sa := httptest.NewServer(a.handler(http.StatusAccepted))
	defer sa.Close()

	c := New(Options{Backoff: time.Millisecond})

	// Primary build fails: nothing is posted, the error is returned.
	boom := errors.New("boom")
	_, err := c.PostAllFunc(context.Background(),
		[]Endpoint{{URL: sa.URL, APIKey: "KEY_A"}},
		func(string) ([]byte, error) { return nil, boom })
	if !errors.Is(err, boom) {
		t.Fatalf("primary build error = %v, want boom", err)
	}
	if _, _, hits := a.get(); hits != 0 {
		t.Errorf("primary hits = %d, want 0 (build failed before post)", hits)
	}

	// Additional build fails: the primary still ships and the call succeeds.
	eps := []Endpoint{{URL: sa.URL, APIKey: "KEY_A"}, {URL: "http://127.0.0.1:0", APIKey: "KEY_B"}}
	if _, err := c.PostAllFunc(context.Background(), eps, func(key string) ([]byte, error) {
		if key == "KEY_B" {
			return nil, boom
		}
		return []byte("{}"), nil
	}); err != nil {
		t.Fatalf("additional build error must be swallowed, got %v", err)
	}
	if _, _, hits := a.get(); hits != 1 {
		t.Errorf("primary hits = %d, want 1", hits)
	}
}

func TestPostAllEmptyIsNoop(t *testing.T) {
	c := New(Options{})
	if status, err := c.PostAll(context.Background(), nil, []byte("{}")); status != 0 || err != nil {
		t.Errorf("PostAll(nil) = (%d, %v), want (0, nil)", status, err)
	}
}

// Shutdown depends on the backoff sleep honoring cancellation: a Post mid-ladder
// must return promptly, not ride out the remaining waits.
func TestPostHonorsContextDuringBackoff(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	c := New(Options{Backoff: 10 * time.Second, MaxTries: 3})
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	_, err := c.Post(ctx, Endpoint{URL: srv.URL, APIKey: "k"}, []byte("{}"))
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("Post held the caller %v through the backoff, want a prompt return", elapsed)
	}
}

// A URL that cannot form a request can never succeed, so it must fail before
// the retry ladder rather than sleeping through it.
func TestPostBadURLFailsFast(t *testing.T) {
	c := New(Options{Backoff: time.Hour, MaxTries: 4})
	start := time.Now()
	if _, err := c.Post(context.Background(), Endpoint{URL: "http://bad url/x", APIKey: "k"}, nil); err == nil {
		t.Fatal("want an error for an unparsable URL")
	}
	if time.Since(start) > time.Second {
		t.Error("a bad URL burned the retry ladder")
	}
}
