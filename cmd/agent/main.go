// Command agent is a minimal, portable agent for the Datadog backend. Logs and
// metrics (DogStatsD plus the host collectors) are the core, with host metadata
// and a few opt-in extras alongside: Live Processes, the profiling proxy, and
// Plan 9 venti disk metrics.
package main

import (
	"context"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/0intro/dd-micro-agent/internal/config"
	"github.com/0intro/dd-micro-agent/internal/dogstatsd"
	"github.com/0intro/dd-micro-agent/internal/host"
	"github.com/0intro/dd-micro-agent/internal/hostmeta"
	"github.com/0intro/dd-micro-agent/internal/hostname"
	"github.com/0intro/dd-micro-agent/internal/intake"
	"github.com/0intro/dd-micro-agent/internal/logs"
	"github.com/0intro/dd-micro-agent/internal/metrics"
	"github.com/0intro/dd-micro-agent/internal/process"
	"github.com/0intro/dd-micro-agent/internal/profiling"
	"github.com/0intro/dd-micro-agent/internal/venti"
)

func main() {
	cfgPath := flag.String("cfgpath", "/etc/datadog-agent/datadog.yaml", "path to datadog.yaml")
	debug := flag.Bool("debug", false, "enable debug logging")
	flag.Parse()

	level := slog.LevelInfo
	if *debug {
		level = slog.LevelDebug
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
	slog.SetDefault(log)

	if err := run(*cfgPath, log); err != nil {
		log.Error("agent stopped", "err", err)
		os.Exit(1)
	}
}

func run(cfgPath string, log *slog.Logger) error {
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return err
	}
	if err := cfg.Validate(); err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	hostName := hostname.Resolve(cfg.Hostname, cfg.HostnameFile)
	client := intake.New(intake.Options{
		SkipSSLValidation: cfg.SkipSSLValidation,
		Proxy:             cfg.Proxy.HTTPS,
		Logger:            log,
	})

	log.Info("agent starting", "hostname", hostName, "site", cfg.Site, "logs_enabled", cfg.LogsEnabled)

	// Metrics: host collectors feed the aggregator as a SeriesSource. DogStatsD
	// feeds it samples. The aggregator owns the flush.
	hostCollector := host.New(host.Options{Logger: log})
	agg := metrics.NewAggregator(metrics.AggregatorOptions{
		Hostname:          hostName,
		Tags:              cfg.Tags,
		Sources:           []metrics.SeriesSource{hostCollector, metrics.HeartbeatSource{}},
		Client:            client,
		Endpoints:         cfg.MetricsEndpoints(),
		CheckRunEndpoints: cfg.CheckRunEndpoints(),
		EventsEndpoints:   cfg.EventsEndpoints(),
		Logger:            log,
	})

	var wg sync.WaitGroup
	errc := make(chan error, 1)

	wg.Add(1)
	go func() {
		defer wg.Done()
		agg.Run(ctx)
	}()

	if cfg.EnableMetadataCollection {
		reporter := hostmeta.New(hostmeta.Options{
			Client:            client,
			IntakeEndpoints:   cfg.IntakeEndpoints(),
			MetadataEndpoints: cfg.MetadataEndpoints(),
			Hostname:          hostName,
			Tags:              cfg.Tags,
			Logger:            log,
		})
		wg.Add(1)
		go func() {
			defer wg.Done()
			reporter.Run(ctx)
		}()
	}

	// Plan 9 venti disk usage, opt-in via venti_url. It runs on its own goroutine
	// (it does blocking HTTP) so it never stalls the metrics flush.
	if cfg.VentiURL != "" {
		reporter := venti.New(venti.Options{
			Client:           client,
			MetricsEndpoints: cfg.MetricsEndpoints(),
			VentiURL:         cfg.VentiURL,
			Hostname:         hostName,
			Tags:             cfg.Tags,
			Logger:           log,
		})
		wg.Add(1)
		go func() {
			defer wg.Done()
			reporter.Run(ctx)
		}()
	}

	// Live Processes, opt-in via process_config.process_collection.enabled. Like
	// venti it runs on its own goroutine: it speaks protobuf to its own intake
	// (process.<site>) and must not stall the metrics flush.
	if cfg.ProcessEnabled() {
		reporter := process.New(process.Options{
			Endpoints:         cfg.ProcessEndpoints(),
			Hostname:          hostName,
			Proxy:             cfg.Proxy.HTTPS,
			SkipSSLValidation: cfg.SkipSSLValidation,
			Logger:            log,
		})
		wg.Add(1)
		go func() {
			defer wg.Done()
			reporter.Run(ctx)
		}()
	}

	if cfg.DogstatsdPort > 0 || cfg.DogstatsdSocket != "" {
		server := dogstatsd.New(dogstatsd.Options{
			Port:         cfg.DogstatsdPort,
			Socket:       cfg.DogstatsdSocket,
			NonLocal:     cfg.DogstatsdNonLocalTraffic,
			Sink:         agg.Add,
			ServiceCheck: agg.AddServiceCheck,
			Event:        agg.AddEvent,
			Logger:       log,
		})
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := server.Run(ctx); err != nil {
				select {
				case errc <- err:
				default:
				}
				stop() // a bind failure should bring the agent down
			}
		}()
	}

	// Profiling proxy, opt-in via apm_config.enabled. It is the agent's only
	// inbound HTTP server: it forwards profiler uploads to the Datadog profiling
	// intake, like the stock trace-agent. A bind failure brings the agent down.
	if cfg.APMEnabled() {
		proxy, err := profiling.New(profiling.Options{
			ListenHost:        cfg.APMConfig.ReceiverHost,
			ListenPort:        cfg.APMConfig.ReceiverPort,
			NonLocal:          cfg.APMConfig.NonLocalTraffic,
			Endpoints:         cfg.ProfilingEndpoints(),
			Hostname:          hostName,
			Env:               cfg.DefaultEnv(),
			Proxy:             cfg.Proxy.HTTPS,
			SkipSSLValidation: cfg.SkipSSLValidation,
			Logger:            log,
		})
		if err != nil {
			stop()
			wg.Wait()
			return err
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := proxy.Run(ctx); err != nil {
				select {
				case errc <- err:
				default:
				}
				stop()
			}
		}()
	}

	if cfg.LogsEnabled {
		if err := startLogs(ctx, &wg, cfg, client, hostName, log); err != nil {
			stop()
			wg.Wait()
			return err
		}
	}

	<-ctx.Done()
	stop() // restore default signal handling, so a second signal kills a wedged drain
	log.Info("agent stopping")
	wg.Wait()

	select {
	case err := <-errc:
		return err
	default:
		return nil
	}
}

func startLogs(ctx context.Context, wg *sync.WaitGroup, cfg *config.Config, client *intake.Client, hostName string, log *slog.Logger) error {
	sources, skipped, err := config.LoadLogSources(cfg.ConfdPath)
	if err != nil {
		return err
	}
	for _, s := range skipped {
		log.Warn("ignoring unsupported log source (only file and journald are supported)", "detail", s)
	}
	if len(sources) == 0 {
		log.Warn("logs_enabled but no log sources found", "confd_path", cfg.ConfdPath)
	}

	agent := logs.NewAgent(logs.Options{
		Sources:             sources,
		Client:              client,
		Endpoints:           cfg.LogsEndpoints(),
		Hostname:            hostName,
		Tags:                cfg.Tags,
		RunPath:             cfg.RunPath,
		BatchWait:           time.Duration(cfg.LogsConfig.BatchWait) * time.Second,
		BatchMaxSize:        cfg.LogsConfig.BatchMaxSize,
		BatchMaxContentSize: cfg.LogsConfig.BatchMaxContentSize,
		Logger:              log,
	})
	wg.Add(1)
	go func() {
		defer wg.Done()
		agent.Run(ctx)
	}()
	return nil
}
