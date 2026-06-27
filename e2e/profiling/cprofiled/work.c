/*
 * work is a CPU-bound program for the profiling e2e. ddprof samples it with
 * perf_event_open and the micro-agent proxy forwards the native profile to
 * Datadog. It spins in named functions so the flame graph has recognizable
 * frames. Build with -O0 -g so the symbols survive. The run time in seconds is
 * argv[1] (default 40).
 */
#include <stdint.h>
#include <stdlib.h>
#include <time.h>

static uint64_t fib(int n) {
	if (n < 2)
		return n;
	return fib(n - 1) + fib(n - 2);
}

static double crunch(void) {
	double s = 0;
	for (int i = 1; i < 2000000; i++)
		s += 1.0 / (double)i;
	return s;
}

int main(int argc, char **argv) {
	int seconds = argc > 1 ? atoi(argv[1]) : 40;
	time_t end = time(NULL) + seconds;
	volatile uint64_t sink = 0;
	while (time(NULL) < end) {
		sink += fib(30);
		sink += (uint64_t)crunch();
	}
	return 0; /* sink is volatile, so the loop is not optimized away */
}
