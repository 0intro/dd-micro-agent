package dogstatsd

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/0intro/dd-micro-agent/internal/metrics"
)

func TestHandlePacket(t *testing.T) {
	var got []metrics.Sample
	s := New(Options{Sink: func(sm metrics.Sample) { got = append(got, sm) }})

	// CRLF line ending, an empty line, a malformed line, and no trailing newline.
	s.handlePacket([]byte("a:1|g\r\nb:2|c|#x:y\n\nnot a metric\nc:3|g"))

	if len(got) != 3 {
		t.Fatalf("got %d samples, want 3: %+v", len(got), got)
	}
	if got[0].Name != "a" || got[1].Name != "b" || got[2].Name != "c" {
		t.Errorf("names = %q, %q, %q", got[0].Name, got[1].Name, got[2].Name)
	}
	if len(got[1].Tags) != 1 || got[1].Tags[0] != "x:y" {
		t.Errorf("b tags = %v", got[1].Tags)
	}
}

func TestHandlePacketRoutesChecksAndEvents(t *testing.T) {
	var (
		samples []metrics.Sample
		checks  []metrics.ServiceCheck
		events  []metrics.Event
	)
	s := New(Options{
		Sink:         func(m metrics.Sample) { samples = append(samples, m) },
		ServiceCheck: func(c metrics.ServiceCheck) { checks = append(checks, c) },
		Event:        func(e metrics.Event) { events = append(events, e) },
	})
	s.handlePacket([]byte("a:1|c\n_sc|up|0\n_e{2,2}:ti|tx\nm:1:2|h\n"))

	if len(samples) != 3 {
		t.Errorf("samples = %d, want 3 (one counter plus two packed histogram values)", len(samples))
	}
	if len(checks) != 1 || checks[0].Check != "up" {
		t.Errorf("checks = %+v, want one for 'up'", checks)
	}
	if len(events) != 1 || events[0].Title != "ti" {
		t.Errorf("events = %+v, want one titled 'ti'", events)
	}
}

func TestServerUDPRoundTrip(t *testing.T) {
	// Reserve an ephemeral UDP port, then hand it to the server.
	probe, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := probe.LocalAddr().(*net.UDPAddr).Port
	probe.Close()

	samples := make(chan metrics.Sample, 4)
	srv := New(Options{Port: port, Sink: func(s metrics.Sample) { samples <- s }})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errc := make(chan error, 1)
	go func() { errc <- srv.Run(ctx) }()

	conn, err := net.Dial("udp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	deadline := time.Now().Add(2 * time.Second)
	for {
		// The write fails with connection refused until the listener is up, so
		// keep sending until a sample comes back.
		conn.Write([]byte("page.views:42|g\n"))
		select {
		case s := <-samples:
			if s.Name != "page.views" || s.Value != 42 {
				t.Errorf("received %+v", s)
			}
			cancel()
			if err := <-errc; err != nil {
				t.Errorf("Run returned %v", err)
			}
			return
		case <-time.After(50 * time.Millisecond):
			if time.Now().After(deadline) {
				t.Fatal("timed out waiting for the UDP sample")
			}
		}
	}
}

func TestServerUnixgramRoundTrip(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "dsd.sock")
	samples := make(chan metrics.Sample, 4)
	srv := New(Options{Socket: sock, Sink: func(s metrics.Sample) { samples <- s }})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errc := make(chan error, 1)
	go func() { errc <- srv.Run(ctx) }()

	waitFor(t, func() bool { _, err := os.Stat(sock); return err == nil })

	conn, err := net.DialUnix("unixgram", nil, &net.UnixAddr{Name: sock, Net: "unixgram"})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	if _, err := conn.Write([]byte("page.views:42|g|#env:prod\n")); err != nil {
		t.Fatalf("write: %v", err)
	}

	select {
	case s := <-samples:
		if s.Name != "page.views" || s.Value != 42 || len(s.Tags) != 1 || s.Tags[0] != "env:prod" {
			t.Errorf("received %+v", s)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for sample")
	}

	cancel()
	if err := <-errc; err != nil {
		t.Errorf("Run returned %v", err)
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition not met within deadline")
}
