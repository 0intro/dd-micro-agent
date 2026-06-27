package profiler_test

import "github.com/0intro/dd-micro-agent/profiler"

// Start the profiler at the top of main and stop it on shutdown. Profiles upload
// through the local agent's proxy (apm_config.enabled) every period.
func Example() {
	err := profiler.Start(
		profiler.WithService("my-service"),
		profiler.WithEnv("prod"),
		profiler.WithVersion("1.0.0"),
		profiler.WithAgentAddr("127.0.0.1:8126"),
	)
	if err != nil {
		panic(err)
	}
	defer profiler.Stop()

	// ... run the program ...
}
