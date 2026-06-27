// Command goprofiled profiles itself with dd-trace-go and uploads through a
// Datadog agent. The e2e points it at the micro-agent's profiling proxy to prove
// the proxy forwards a real Go profile to the intake. It is a separate module so
// the agent's own go.mod keeps its zero-dependency charter.
package main

import (
	"log"
	"os"
	"time"

	"github.com/DataDog/dd-trace-go/v2/profiler"
)

func main() {
	addr := env("PROFILE_AGENT_ADDR", "127.0.0.1:8126")
	runFor := 40 * time.Second
	if d, err := time.ParseDuration(os.Getenv("PROFILE_DURATION")); err == nil {
		runFor = d
	}

	opts := []profiler.Option{
		profiler.WithService(env("DD_SERVICE", "microagent-e2e-go")),
		profiler.WithEnv(env("DD_ENV", "e2e")),
		profiler.WithVersion(env("DD_VERSION", "0.0.1")),
		profiler.WithAgentAddr(addr),
		profiler.WithProfileTypes(profiler.CPUProfile, profiler.HeapProfile),
		profiler.WithPeriod(15 * time.Second),
		profiler.WithUploadTimeout(20 * time.Second),
	}
	if tag := os.Getenv("PROFILE_TAG"); tag != "" {
		opts = append(opts, profiler.WithTags(tag))
	}
	if err := profiler.Start(opts...); err != nil {
		log.Fatalf("profiler.Start: %v", err)
	}
	defer profiler.Stop()

	log.Printf("profiling for %s, uploading via %s", runFor, addr)
	deadline := time.Now().Add(runFor)
	var sink []byte
	for time.Now().Before(deadline) {
		sink = burn(sink)
	}
	log.Printf("done, last buffer %d bytes", len(sink))
}

// burn spends CPU and allocates so the cpu and heap profiles are not empty. The
// buffer grows then resets, bounding memory while keeping the allocator busy.
func burn(prev []byte) []byte {
	sum := 0
	for i := 0; i < 5_000_000; i++ {
		sum += i * i
	}
	buf := make([]byte, 1<<20)
	buf[sum%len(buf)] = 1
	if len(prev) > 1<<23 {
		return buf
	}
	return append(prev, buf...)
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
