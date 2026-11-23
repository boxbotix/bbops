package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/boxbotix/bbops/internal/host"
)

var (
	version = "dev"
	commit  = "unknown"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	os.Exit(runContext(ctx, os.Args[1:], os.Stdout, os.Stderr, version, commit))
}

func run(args []string, stdout, stderr io.Writer, ver, sha string) int {
	return runContext(context.Background(), args, stdout, stderr, ver, sha)
}

func runContext(ctx context.Context, args []string, stdout, stderr io.Writer, ver, sha string) int {
	fs := flag.NewFlagSet("bbops", flag.ContinueOnError)
	fs.SetOutput(stderr)

	var (
		showVersion     = fs.Bool("version", false, "print bbops version and commit")
		validateOnly    = fs.Bool("validate", false, "validate SemEngine configuration and composition offline")
		configPath      = fs.String("config", "", "path to SemEngine flow configuration JSON (empty uses embedded default)")
		dataDir         = fs.String("data-dir", ".bbops/data", "JetStream data directory for embedded NATS")
		natsURL         = fs.String("nats-url", "", "external NATS server URL (empty starts embedded NATS + JetStream)")
		uiDir           = fs.String("ui-dir", "ui/build", "compiled SvelteKit static UI directory")
		httpPort        = fs.Int("http-port", 0, "override service-manager HTTP port (1024..65535)")
		telemetryWSPort = fs.Int("telemetry-ws-port", 0, "override telemetry output/websocket port (1024..65535)")
		graphWSPort     = fs.Int("graph-ws-port", 0, "override SKG ENTITY_STATES output/websocket port (1024..65535)")
		metricsPort     = fs.Int("metrics-port", 0, "override Prometheus metrics port (1024..65535; <0 disables)")
		bindHost        = fs.String("bind-host", "", "override output/websocket bind host (e.g. 127.0.0.1)")
		shutdownTimeout = fs.Duration("shutdown-timeout", 10*time.Second, "graceful shutdown timeout")
	)

	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 0 {
		_, _ = fmt.Fprintf(stderr, "unexpected arguments: %v\n", fs.Args())
		return 2
	}
	if *showVersion {
		_, _ = fmt.Fprintf(stdout, "bbops %s (%s)\n", ver, sha)
		return 0
	}

	overrides := host.PortOverrides{
		HTTPPort:        *httpPort,
		TelemetryWSPort: *telemetryWSPort,
		GraphWSPort:     *graphWSPort,
		MetricsPort:     *metricsPort,
		BindHost:        *bindHost,
	}

	if *validateOnly {
		cfg, err := host.LoadConfig(*configPath, overrides)
		if err != nil {
			_, _ = fmt.Fprintf(stderr, "config error: %v\n", err)
			return 1
		}
		if _, err := host.ValidateOffline(cfg); err != nil {
			_, _ = fmt.Fprintf(stderr, "validation error: %v\n", err)
			return 1
		}
		_, _ = fmt.Fprintln(stdout, "bbops configuration valid")
		return 0
	}

	logger := slog.New(slog.NewTextHandler(stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	rt, err := host.Start(ctx, host.Options{
		ConfigPath: *configPath,
		DataDir:    *dataDir,
		NATSURL:    *natsURL,
		UIDir:      *uiDir,
		Version:    ver,
		Overrides:  overrides,
		Logger:     logger,
	})
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "startup failed: %v\n", err)
		return 1
	}

	endpoints := rt.Endpoints()
	logger.Info("bbops host ready",
		"version", ver,
		"commit", sha,
		"http", endpoints.HTTPBaseURL,
		"telemetry_ws", endpoints.TelemetryWS,
		"graph_ws", endpoints.GraphWS,
		"nats", rt.NATSURL(),
	)

	<-ctx.Done()

	stopCtx, cancel := context.WithTimeout(context.Background(), *shutdownTimeout)
	defer cancel()
	if err := rt.Stop(stopCtx); err != nil {
		_, _ = fmt.Fprintf(stderr, "shutdown error: %v\n", err)
		return 1
	}
	return 0
}
