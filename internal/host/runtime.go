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

	"github.com/boxbotix/bbops/internal/uxsvocab"
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
	serviceManager atomic.Pointer[service.Manager]
	serviceNames   []string
	mutations      *projection.MutationClient

	onReadinessCheck atomic.Pointer[func()]
	started          atomic.Bool
	initialReady     atomic.Bool
	stopping         atomic.Bool
	stopMu           sync.Mutex
	closed           bool
}

// Start boots embedded NATS + JetStream (unless opts.NATSURL is set), ensures
// the ENTITY stream and kvcatalog retention invariants, starts ConfigManager and
// ServiceManager (including graph-ingest, graph-index, graph-lexical, graph-query,
// graph-gateway, and dual output/websocket planes), and returns once listeners
// are bound. Callers awaiting full ADR-109 component readiness should call WaitReady.
func Start(ctx context.Context, opts Options) (*Runtime, error) {
	if ctx == nil {
		return nil, errors.New("host: nil context")
	}
	baseLogger := opts.Logger
	if baseLogger == nil {
		baseLogger = slog.Default()
	}

	// Validate both the persisted base flow and overridden runtime flow through
	// the single shared offline preparation path before starting NATS or touching disk.
	prepared, err := PrepareConfig(opts.ConfigPath, opts.Overrides, opts.Version)
	if err != nil {
		return nil, err
	}
	baseCfg := prepared.Base

	rt := &Runtime{}
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
	rawNATSURL := strings.TrimSpace(opts.NATSURL)
	if rawNATSURL == "" {
		srv, err := startEmbeddedNATS(ctx, filepath.Join(dataDir, "jetstream"))
		if err != nil {
			return nil, err
		}
		rt.natsServer = srv
		rawNATSURL = srv.ClientURL()
	}
	redactedNATSURL := RedactNATSURL(rawNATSURL)
	secrets := extractNATSURLSecrets(rawNATSURL)
	logger := newRedactingLogger(baseLogger, secrets)

	rt.natsURL = redactedNATSURL
	baseCfg.NATS.URLs = []string{redactedNATSURL}

	metricsRegistry := metric.NewMetricsRegistry()
	client, err := openNATSClient(ctx, rawNATSURL, logger, secrets, metricsRegistry)
	if err != nil {
		return cleanup(err)
	}
	rt.natsClient = client

	if err := config.NewStreamsManager(client, logger).EnsureStreams(ctx, baseCfg); err != nil {
		return cleanup(fmt.Errorf("ensure JetStream streams: %w", err))
	}

	cfgMgr, err := config.NewConfigManager(baseCfg, client, logger)
	if err != nil {
		return cleanup(fmt.Errorf("create config manager: %w", err))
	}
	if err := cfgMgr.Start(ctx); err != nil {
		return cleanup(fmt.Errorf("start config manager: %w", err))
	}
	rt.configManager = cfgMgr

	// Apply explicit process listener overrides and server version AFTER KV
	// arbitration so warm restarts honor CLI/ephemeral overrides instead of
	// silently binding stale KV ports.
	if err := cfgMgr.GetConfig().Mutate(func(c *config.Config) error {
		if err := ApplyPortOverrides(c, opts.Overrides); err != nil {
			return err
		}
		if opts.Version != "" {
			if err := setServerVersion(c, opts.Version); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		return cleanup(fmt.Errorf("apply process overrides after config arbitration: %w", err))
	}

	effectiveCfg := cfgMgr.GetConfig().Get()
	if _, err := ValidateOffline(effectiveCfg); err != nil {
		return cleanup(fmt.Errorf("validate effective configuration: %w", err))
	}
	endpoints, err := ResolveEndpoints(effectiveCfg)
	if err != nil {
		return cleanup(fmt.Errorf("resolve effective endpoints: %w", err))
	}
	rt.endpoints = endpoints
	rt.platform = types.PlatformMeta{
		Org:      effectiveCfg.Platform.Org,
		Platform: effectiveCfg.Platform.ID,
	}

	resolvedServices, err := service.ResolveServiceConfigs(effectiveCfg.Services)
	if err != nil {
		return cleanup(fmt.Errorf("resolve service configs: %w", err))
	}
	for name, svcCfg := range resolvedServices {
		if name != "service-manager" && svcCfg.Enabled {
			rt.serviceNames = append(rt.serviceNames, name)
		}
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
	if err := uxsvocab.RegisterPayloads(payloadReg); err != nil {
		return cleanup(fmt.Errorf("register uxs payloads: %w", err))
	}
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
	svcMgr.UseHTTPMiddleware(NewStaticUIMiddleware(uiDir, rt.isUIAdmitted))

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
	rt.serviceManager.Store(svcMgr)

	if err := svcMgr.StartAll(ctx); err != nil {
		return cleanup(fmt.Errorf("start services: %w", err))
	}
	rt.started.Store(true)
	_ = rt.checkLiveReadiness()
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

// NATSURL returns the active NATS client URL with any credentials redacted.
func (rt *Runtime) NATSURL() string { return rt.natsURL }

// NATSClient returns the connected SemEngine NATS client.
func (rt *Runtime) NATSClient() *natsclient.Client { return rt.natsClient }

// Platform returns the effective authority identity minted by ConfigManager.
func (rt *Runtime) Platform() types.PlatformMeta { return rt.platform }

// IsReady reports whether all composed SemEngine services and SKG/WebSocket
// components currently satisfy ADR-109 readiness and shutdown has not begun.
func (rt *Runtime) IsReady() bool {
	return rt.checkLiveReadiness()
}

// WaitReady blocks until IsReady reports true, Stop begins, or ctx ends.
func (rt *Runtime) WaitReady(ctx context.Context) error {
	if ctx == nil {
		return errors.New("host: nil context")
	}
	if rt.IsReady() {
		return nil
	}
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		if rt.stopping.Load() {
			return errors.New("host: runtime stopped before becoming ready")
		}
		if rt.IsReady() {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func (rt *Runtime) isUIAdmitted() bool {
	if rt.stopping.Load() {
		return false
	}
	if rt.initialReady.Load() {
		return true
	}
	return rt.checkLiveReadiness()
}

func (rt *Runtime) checkLiveReadiness() bool {
	if !rt.started.Load() || rt.stopping.Load() {
		return false
	}
	if hookPtr := rt.onReadinessCheck.Load(); hookPtr != nil && *hookPtr != nil {
		(*hookPtr)()
	}
	svcMgr := rt.serviceManager.Load()
	if svcMgr == nil {
		return false
	}
	for _, name := range rt.serviceNames {
		svc, ok := svcMgr.GetService(name)
		if !ok || svc.Status() != service.StatusRunning || !svc.IsHealthy() {
			return false
		}
	}
	cmSvc, ok := svcMgr.GetService("component-manager")
	if !ok {
		return false
	}
	cm, ok := cmSvc.(*service.ComponentManager)
	if !ok {
		return false
	}
	for _, status := range cm.GetComponentHealth() {
		if !status.Healthy {
			return false
		}
	}
	if rt.stopping.Load() {
		return false
	}
	rt.initialReady.Store(true)
	return true
}

// Stop shuts down ServiceManager, ConfigManager, the NATS client, and any
// embedded NATS server in reverse boot order. It is idempotent once nil is returned.
func (rt *Runtime) Stop(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	rt.stopping.Store(true)
	rt.initialReady.Store(false)

	rt.stopMu.Lock()
	defer rt.stopMu.Unlock()
	if rt.closed {
		return nil
	}

	var errs []error
	if svcMgr := rt.serviceManager.Load(); svcMgr != nil {
		if err := svcMgr.StopAll(ctx); err != nil {
			errs = append(errs, fmt.Errorf("stop service manager: %w", err))
		} else {
			rt.serviceManager.Store(nil)
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

func openNATSClient(ctx context.Context, rawURL string, logger *slog.Logger, secrets []string, metrics *metric.MetricsRegistry) (*natsclient.Client, error) {
	opts := []natsclient.ClientOption{
		natsclient.WithLogger(logger),
	}
	if metrics != nil {
		opts = append(opts, natsclient.WithMetrics(metrics))
	}
	client, err := natsclient.NewClient(rawURL, opts...)
	if err != nil {
		return nil, fmt.Errorf("create NATS client: %w", scrubSecretsError(err, secrets))
	}
	if err := client.Connect(ctx); err != nil {
		_ = client.Close(ctx)
		return nil, fmt.Errorf("connect NATS client: %w", scrubSecretsError(err, secrets))
	}
	waitCtx, cancel := context.WithTimeout(ctx, natsConnectBound)
	defer cancel()
	if err := client.WaitForConnection(waitCtx); err != nil {
		_ = client.Close(ctx)
		return nil, fmt.Errorf("wait for NATS connection: %w", scrubSecretsError(err, secrets))
	}
	return client, nil
}

func scrubSecretsError(err error, secrets []string) error {
	if err == nil || len(secrets) == 0 {
		return err
	}
	msg := err.Error()
	scrubbed := scrubStringSecrets(msg, secrets)
	if scrubbed == msg {
		return err
	}
	return errors.New(scrubbed)
}

func scrubStringSecrets(s string, secrets []string) string {
	out := s
	for _, secret := range secrets {
		if secret == "" || !strings.Contains(out, secret) {
			continue
		}
		if strings.Contains(secret, "://") || strings.Contains(secret, "@") {
			out = strings.ReplaceAll(out, secret, RedactNATSURL(secret))
		} else {
			out = strings.ReplaceAll(out, secret, "<redacted>")
		}
	}
	return out
}

type redactingSlogHandler struct {
	inner   slog.Handler
	secrets []string
}

func newRedactingLogger(base *slog.Logger, secrets []string) *slog.Logger {
	if base == nil {
		base = slog.Default()
	}
	return slog.New(&redactingSlogHandler{
		inner:   base.Handler(),
		secrets: secrets,
	})
}

func (h *redactingSlogHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.inner.Enabled(ctx, level)
}

func (h *redactingSlogHandler) Handle(ctx context.Context, rec slog.Record) error {
	clean := slog.NewRecord(rec.Time, rec.Level, scrubStringSecrets(rec.Message, h.secrets), rec.PC)
	rec.Attrs(func(a slog.Attr) bool {
		clean.AddAttrs(h.scrubAttr(a))
		return true
	})
	return h.inner.Handle(ctx, clean)
}

func (h *redactingSlogHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	cleaned := make([]slog.Attr, len(attrs))
	for i, a := range attrs {
		cleaned[i] = h.scrubAttr(a)
	}
	return &redactingSlogHandler{
		inner:   h.inner.WithAttrs(cleaned),
		secrets: h.secrets,
	}
}

func (h *redactingSlogHandler) WithGroup(name string) slog.Handler {
	return &redactingSlogHandler{
		inner:   h.inner.WithGroup(name),
		secrets: h.secrets,
	}
}

func (h *redactingSlogHandler) scrubAttr(a slog.Attr) slog.Attr {
	val := a.Value.Resolve()
	switch val.Kind() {
	case slog.KindString:
		s := val.String()
		switch strings.ToLower(a.Key) {
		case "urls", "url", "nats":
			s = RedactNATSURL(s)
		}
		return slog.String(a.Key, scrubStringSecrets(s, h.secrets))
	case slog.KindGroup:
		group := val.Group()
		cleaned := make([]any, len(group))
		for i, ga := range group {
			cleaned[i] = h.scrubAttr(ga)
		}
		return slog.Group(a.Key, cleaned...)
	case slog.KindAny:
		if errVal, ok := val.Any().(error); ok && errVal != nil {
			return slog.Any(a.Key, scrubSecretsError(errVal, h.secrets))
		}
	}
	return a
}

func setServerVersion(cfg *config.Config, ver string) error {
	if cfg == nil {
		return errors.New("host: nil configuration")
	}
	if cfg.Services == nil {
		cfg.Services = make(types.ServiceConfigs)
	}
	svc, exists := cfg.Services["service-manager"]
	parsed := map[string]any{}
	if exists && !isEmptyOrNullJSON(svc.Config) {
		if err := json.Unmarshal(svc.Config, &parsed); err != nil {
			return fmt.Errorf("decode service-manager config: %w", err)
		}
		if parsed == nil {
			parsed = map[string]any{}
		}
	}
	if rawInfo, hasInfo := parsed["server_info"]; hasInfo && rawInfo != nil {
		infoMap, ok := rawInfo.(map[string]any)
		if !ok {
			return errors.New("service-manager.server_info must be a JSON object")
		}
		infoMap["version"] = ver
		parsed["server_info"] = infoMap
	} else {
		parsed["server_info"] = map[string]any{"version": ver}
	}
	if !exists {
		svc.Enabled = true
	}
	encoded, err := json.Marshal(parsed)
	if err != nil {
		return err
	}
	svc.Config = encoded
	cfg.Services["service-manager"] = svc
	return nil
}
