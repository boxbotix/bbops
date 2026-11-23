package host

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/c360studio/semengine/component"
	"github.com/c360studio/semengine/config"
	"github.com/c360studio/semengine/graph/kvcatalog"
	"github.com/c360studio/semengine/metric"
	"github.com/c360studio/semengine/natsclient"
	"github.com/c360studio/semengine/payloadregistry"
	"github.com/c360studio/semengine/pkg/projection"
	"github.com/c360studio/semengine/service"
	"github.com/c360studio/semengine/types"
	natsserver "github.com/nats-io/nats-server/v2/server"
)

const (
	defaultDataDir         = ".bbops/data"
	defaultUIDir           = "ui/build"
	embeddedNATSReadyBound = 10 * time.Second
	natsConnectBound       = 10 * time.Second
)

// Options configures a bbops embedded SemEngine + NATS/JetStream runtime instance.
type Options struct {
	ConfigPath       string
	DataDir          string
	NATSURL          string
	UIDir            string
	Version          string
	Overrides        PortOverrides
	Logger           *slog.Logger
	RegisterPayloads func(*payloadregistry.Registry) error
	Contracts        []projection.Contract
}

// Runtime owns a started bbops host instance (embedded NATS/JetStream,
// ConfigManager, ServiceManager, and ADR-115 HTTP/WS surfaces).
type Runtime struct {
	endpoints      ResolvedEndpoints
	natsURL        string
	platform       types.PlatformMeta
	natsServer     *natsserver.Server
	natsClient     *natsclient.Client
	configManager  *config.Manager
	serviceManager *service.Manager
	mutations      *projection.MutationClient

	ready  atomic.Bool
	stopMu sync.Mutex
	closed bool
}

// Start boots embedded NATS + JetStream (unless opts.NATSURL is set), ensures
// the ENTITY stream and kvcatalog retention invariants, starts ConfigManager and
// ServiceManager (including graph-ingest, graph-index, graph-lexical, graph-query,
// graph-gateway, and dual output/websocket planes), and returns once ready.
func Start(ctx context.Context, opts Options) (*Runtime, error) {
	if ctx == nil {
		return nil, errors.New("host: nil context")
	}
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}

	cfg, err := LoadConfig(opts.ConfigPath, opts.Overrides)
	if err != nil {
		return nil, err
	}
	if opts.Version != "" {
		_ = setServerVersion(cfg, opts.Version)
	}
	if _, err := ValidateOffline(cfg); err != nil {
		return nil, err
	}
	endpoints, err := ResolveEndpoints(cfg)
	if err != nil {
		return nil, err
	}

	rt := &Runtime{endpoints: endpoints}
	cleanup := func(cause error) (*Runtime, error) {
		stopCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = rt.Stop(stopCtx)
		return nil, cause
	}

	dataDir := opts.DataDir
	if dataDir == "" {
		dataDir = defaultDataDir
	}
	natsURL := strings.TrimSpace(opts.NATSURL)
	if natsURL == "" {
		srv, err := startEmbeddedNATS(ctx, filepath.Join(dataDir, "jetstream"))
		if err != nil {
			return nil, err
		}
		rt.natsServer = srv
		natsURL = srv.ClientURL()
	}
	rt.natsURL = natsURL
	cfg.NATS.URLs = []string{natsURL}

	metricsRegistry := metric.NewMetricsRegistry()
	client, err := openNATSClient(ctx, natsURL, metricsRegistry)
	if err != nil {
		return cleanup(err)
	}
	rt.natsClient = client

	if err := config.NewStreamsManager(client, logger).EnsureStreams(ctx, cfg); err != nil {
		return cleanup(fmt.Errorf("ensure JetStream streams: %w", err))
	}

	cfgMgr, err := config.NewConfigManager(cfg, client, logger)
	if err != nil {
		return cleanup(fmt.Errorf("create config manager: %w", err))
	}
	if err := cfgMgr.Start(ctx); err != nil {
		return cleanup(fmt.Errorf("start config manager: %w", err))
	}
	rt.configManager = cfgMgr

	effectiveCfg := cfgMgr.GetConfig().Get()
	rt.platform = types.PlatformMeta{
		Org:      effectiveCfg.Platform.Org,
		Platform: effectiveCfg.Platform.ID,
	}

	if len(opts.Contracts) > 0 {
		mutClient, err := service.WireGraphRuntime(ctx, client, logger, opts.Contracts...)
		if err != nil {
			return cleanup(fmt.Errorf("wire graph runtime: %w", err))
		}
		rt.mutations = mutClient
	} else if err := kvcatalog.EnsureCatalogRetentionClean(ctx, client, logger); err != nil {
		return cleanup(fmt.Errorf("ensure catalog retention clean: %w", err))
	}

	compReg := component.NewRegistry()
	if err := RegisterComponents(compReg); err != nil {
		return cleanup(err)
	}

	payloadReg := payloadregistry.New()
	if opts.RegisterPayloads != nil {
		if err := opts.RegisterPayloads(payloadReg); err != nil {
			return cleanup(fmt.Errorf("register payloads: %w", err))
		}
	}

	svcReg := service.NewServiceRegistry()
	if err := service.RegisterAll(svcReg); err != nil {
		return cleanup(fmt.Errorf("register framework services: %w", err))
	}

	uiDir := opts.UIDir
	if uiDir == "" {
		uiDir = defaultUIDir
	}

	svcMgr := service.NewServiceManager(svcReg)
	svcMgr.UseHTTPMiddleware(NewStaticUIMiddleware(uiDir, rt.ready.Load))

	svcDeps := &service.Dependencies{
		NATSClient:        client,
		MetricsRegistry:   metricsRegistry,
		Logger:            logger,
		Platform:          rt.platform,
		Manager:           cfgMgr,
		ComponentRegistry: compReg,
		PayloadRegistry:   payloadReg,
	}
	if err := svcMgr.ConfigureFromServices(effectiveCfg.Services, svcDeps); err != nil {
		return cleanup(fmt.Errorf("configure services: %w", err))
	}
	rt.serviceManager = svcMgr

	if err := svcMgr.StartAll(ctx); err != nil {
		return cleanup(fmt.Errorf("start services: %w", err))
	}
	rt.ready.Store(true)
	return rt, nil
}

// StartEphemeral allocates free loopback ports in 1024..65535 and starts Runtime,
// retrying up to maxAttempts when a concurrent process claims an ephemeral port
// between probing and bind.
func StartEphemeral(ctx context.Context, opts Options, maxAttempts int) (*Runtime, error) {
	if maxAttempts < 1 {
		maxAttempts = 1
	}
	var lastErr error
	for attempt := 0; attempt < maxAttempts; attempt++ {
		ports, err := AllocateLoopbackPorts(4)
		if err != nil {
			return nil, err
		}
		tryOpts := opts
		tryOpts.Overrides.BindHost = "127.0.0.1"
		tryOpts.Overrides.HTTPPort = ports[0]
		tryOpts.Overrides.TelemetryWSPort = ports[1]
		tryOpts.Overrides.GraphWSPort = ports[2]
		if tryOpts.Overrides.MetricsPort == 0 {
			tryOpts.Overrides.MetricsPort = ports[3]
		}
		rt, err := Start(ctx, tryOpts)
		if err == nil {
			return rt, nil
		}
		lastErr = err
		if !errors.Is(err, syscall.EADDRINUSE) && !strings.Contains(err.Error(), "address already in use") {
			return nil, err
		}
	}
	return nil, lastErr
}

// AllocateLoopbackPorts reserves count distinct free loopback TCP ports in
// 1024..65535 simultaneously before releasing them.
func AllocateLoopbackPorts(count int) ([]int, error) {
	if count <= 0 {
		return nil, errors.New("port count must be positive")
	}
	listeners := make([]net.Listener, 0, count)
	defer func() {
		for _, ln := range listeners {
			_ = ln.Close()
		}
	}()
	ports := make([]int, 0, count)
	for range count {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			return nil, fmt.Errorf("allocate loopback port: %w", err)
		}
		listeners = append(listeners, ln)
		addr, ok := ln.Addr().(*net.TCPAddr)
		if !ok || addr.Port < minUserPort || addr.Port > maxUserPort {
			return nil, fmt.Errorf("allocated port out of user range: %v", ln.Addr())
		}
		ports = append(ports, addr.Port)
	}
	return ports, nil
}

// Endpoints returns the resolved HTTP and WebSocket URLs of the started runtime.
func (rt *Runtime) Endpoints() ResolvedEndpoints { return rt.endpoints }

// NATSURL returns the active NATS client URL (embedded or external).
func (rt *Runtime) NATSURL() string { return rt.natsURL }

// NATSClient returns the connected SemEngine NATS client.
func (rt *Runtime) NATSClient() *natsclient.Client { return rt.natsClient }

// Platform returns the effective authority identity minted by ConfigManager.
func (rt *Runtime) Platform() types.PlatformMeta { return rt.platform }

// Stop shuts down ServiceManager, ConfigManager, the NATS client, and any
// embedded NATS server in reverse boot order. It is idempotent once nil is returned.
func (rt *Runtime) Stop(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	rt.ready.Store(false)

	rt.stopMu.Lock()
	defer rt.stopMu.Unlock()
	if rt.closed {
		return nil
	}

	var errs []error
	if rt.serviceManager != nil {
		if err := rt.serviceManager.StopAll(ctx); err != nil {
			errs = append(errs, fmt.Errorf("stop service manager: %w", err))
		} else {
			rt.serviceManager = nil
		}
	}
	if rt.configManager != nil {
		if err := rt.configManager.Stop(ctx); err != nil {
			errs = append(errs, fmt.Errorf("stop config manager: %w", err))
		} else {
			rt.configManager = nil
		}
	}
	if rt.natsClient != nil {
		if err := rt.natsClient.Close(ctx); err != nil {
			errs = append(errs, fmt.Errorf("close NATS client: %w", err))
		} else {
			rt.natsClient = nil
		}
	}
	if rt.natsServer != nil {
		done := make(chan struct{})
		srv := rt.natsServer
		go func() {
			srv.Shutdown()
			srv.WaitForShutdown()
			close(done)
		}()
		select {
		case <-done:
			rt.natsServer = nil
		case <-ctx.Done():
			errs = append(errs, fmt.Errorf("wait for embedded NATS shutdown: %w", ctx.Err()))
		}
	}

	if len(errs) == 0 {
		rt.closed = true
	}
	return errors.Join(errs...)
}

func startEmbeddedNATS(ctx context.Context, storeDir string) (*natsserver.Server, error) {
	srv, err := natsserver.NewServer(&natsserver.Options{
		Host:      "127.0.0.1",
		Port:      -1,
		NoLog:     true,
		NoSigs:    true,
		JetStream: true,
		StoreDir:  storeDir,
	})
	if err != nil {
		return nil, fmt.Errorf("configure embedded NATS server: %w", err)
	}
	srv.Start()

	deadline := embeddedNATSReadyBound
	if dl, ok := ctx.Deadline(); ok {
		if rem := time.Until(dl); rem > 0 && rem < deadline {
			deadline = rem
		}
	}
	if !srv.ReadyForConnections(deadline) {
		srv.Shutdown()
		srv.WaitForShutdown()
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("embedded NATS canceled during startup: %w", err)
		}
		return nil, errors.New("embedded NATS server did not become ready within deadline")
	}
	return srv, nil
}

func openNATSClient(ctx context.Context, url string, metrics *metric.MetricsRegistry) (*natsclient.Client, error) {
	var opts []natsclient.ClientOption
	if metrics != nil {
		opts = append(opts, natsclient.WithMetrics(metrics))
	}
	client, err := natsclient.NewClient(url, opts...)
	if err != nil {
		return nil, fmt.Errorf("create NATS client: %w", err)
	}
	if err := client.Connect(ctx); err != nil {
		_ = client.Close(ctx)
		return nil, fmt.Errorf("connect NATS client: %w", err)
	}
	waitCtx, cancel := context.WithTimeout(ctx, natsConnectBound)
	defer cancel()
	if err := client.WaitForConnection(waitCtx); err != nil {
		_ = client.Close(ctx)
		return nil, fmt.Errorf("wait for NATS connection: %w", err)
	}
	return client, nil
}

func setServerVersion(cfg *config.Config, ver string) error {
	svc, ok := cfg.Services["service-manager"]
	if !ok || len(svc.Config) == 0 {
		return nil
	}
	var parsed map[string]any
	if err := json.Unmarshal(svc.Config, &parsed); err != nil {
		return err
	}
	info, _ := parsed["server_info"].(map[string]any)
	if info == nil {
		info = map[string]any{}
	}
	info["version"] = ver
	parsed["server_info"] = info
	encoded, err := json.Marshal(parsed)
	if err != nil {
		return err
	}
	svc.Config = encoded
	cfg.Services["service-manager"] = svc
	return nil
}
