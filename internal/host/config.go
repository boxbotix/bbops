package host

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/boxbotix/bbops/configs"
	"github.com/boxbotix/bbops/internal/control"
	"github.com/boxbotix/bbops/internal/mavlink"
	"github.com/boxbotix/bbops/internal/uxsvocab"
	"github.com/c360studio/semengine/component"
	"github.com/c360studio/semengine/composition"
	"github.com/c360studio/semengine/config"
	graphgateway "github.com/c360studio/semengine/gateway/graph-gateway"
	"github.com/c360studio/semengine/metric"
	"github.com/c360studio/semengine/natsclient"
	"github.com/c360studio/semengine/output/websocket"
	"github.com/c360studio/semengine/payloadregistry"
	graphindex "github.com/c360studio/semengine/processor/graph-index"
	graphingest "github.com/c360studio/semengine/processor/graph-ingest"
	graphlexical "github.com/c360studio/semengine/processor/graph-lexical"
	graphquery "github.com/c360studio/semengine/processor/graph-query"
	"github.com/c360studio/semengine/service"
	"github.com/c360studio/semengine/types"
)

const (
	// MAVLinkIngestComponent is the external 0xB1 MAVLink v2 UDP 3-lane ingest adapter.
	MAVLinkIngestComponent = mavlink.DefaultInstanceName
	// TelemetryWSComponent is output/websocket instance #1 (core NATS bbops.telemetry.>).
	TelemetryWSComponent = "telemetry-websocket"
	// GraphWSComponent is output/websocket instance #2 (kv-watch ENTITY_STATES).
	GraphWSComponent = "graph-websocket"
	// EntityStreamName is the durable JetStream stream backing entity.> ingest.
	EntityStreamName = "ENTITY"

	minUserPort = 1024
	maxUserPort = 65535
)

var requiredM02Components = []struct {
	instance string
	factory  string
}{
	{instance: "graph-ingest", factory: "graph-ingest"},
	{instance: "graph-index", factory: "graph-index"},
	{instance: "graph-lexical", factory: "graph-lexical"},
	{instance: "graph-query", factory: "graph-query"},
	{instance: "graph-gateway", factory: "graph-gateway"},
	{instance: TelemetryWSComponent, factory: "websocket"},
	{instance: GraphWSComponent, factory: "websocket"},
}

// PortOverrides carries optional CLI or test listener overrides applied atop the
// loaded SemEngine flow configuration.
type PortOverrides struct {
	HTTPPort        int
	TelemetryWSPort int
	GraphWSPort     int
	// MetricsPort overrides the Prometheus metrics listener when > 0, keeps the
	// flow config value when 0, and disables the metrics service when < 0.
	MetricsPort int
	// MAVLinkUDPPort overrides the mavlink-ingest UDP listener when > 0, keeps
	// the flow config value when 0, and disables mavlink-ingest when < 0.
	MAVLinkUDPPort int
	// BindHost overrides output/websocket and UDP bind hosts when non-empty (e.g. "127.0.0.1").
	BindHost string
}

// ResolvedEndpoints holds the effective local URLs and UDP bind address for a configured bbops runtime.
type ResolvedEndpoints struct {
	HTTPPort       int
	MetricsPort    int
	MAVLinkUDPPort int
	HTTPBaseURL    string
	TelemetryWS    string
	GraphWS        string
	MAVLinkUDPAddr string
}

// RegisterComponents registers the SemEngine v0.1.0-alpha.7 SKG, GraphQL gateway,
// dual-WebSocket, and bbops MAVLink v2 UDP ingest component factories.
func RegisterComponents(registry *component.Registry) error {
	return registerComponentsWithHook(registry, nil)
}

func registerComponentsWithHook(registry *component.Registry, onMAVLink func(*mavlink.Component)) error {
	if registry == nil {
		return errors.New("host: nil component registry")
	}
	registrars := []struct {
		name string
		fn   func(*component.Registry) error
	}{
		{
			name: mavlink.FactoryName,
			fn: func(reg *component.Registry) error {
				return mavlink.RegisterWithHook(reg, onMAVLink)
			},
		},
		{name: "graph-ingest", fn: graphingest.Register},
		{name: "graph-index", fn: graphindex.Register},
		{name: "graph-lexical", fn: graphlexical.Register},
		{name: "graph-query", fn: graphquery.Register},
		{name: "graph-gateway", fn: graphgateway.Register},
		{name: "websocket", fn: websocket.Register},
	}
	for _, r := range registrars {
		if err := r.fn(registry); err != nil {
			return fmt.Errorf("register %s: %w", r.name, err)
		}
	}
	return nil
}

// PreparedConfig holds the validated persisted base flow configuration, the
// validated runtime flow configuration after ephemeral overrides and version
// stamping, and its offline composition result.
type PreparedConfig struct {
	Base        *config.Config
	Effective   *config.Config
	Composition *composition.Result
}

// LoadConfig reads path (or the embedded configs/bbops.json default when path is
// empty), parses it via SemEngine's loader, applies the additive pre-M1.2
// mavlink-ingest upgrade in memory when loading a recognized legacy flow file
// (preserving explicit operator values and leaving path untouched on disk), and
// applies any PortOverrides.
func LoadConfig(path string, overrides PortOverrides) (*config.Config, error) {
	var data []byte
	var err error
	if path == "" {
		data = configs.DefaultFlowJSON
	} else {
		data, err = os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read config %q: %w", path, err)
		}
	}
	cfg, err := config.NewLoader().LoadFromBytes(data)
	if err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	if path != "" {
		if err := upgradeLegacyFlowInMemory(cfg); err != nil {
			return nil, err
		}
	}
	if err := ApplyPortOverrides(cfg, overrides); err != nil {
		return nil, err
	}
	return cfg, nil
}

func upgradeLegacyFlowInMemory(cfg *config.Config) error {
	if cfg == nil {
		return nil
	}
	if _, hasMAV := cfg.Components[MAVLinkIngestComponent]; hasMAV {
		return nil
	}
	for _, req := range requiredM02Components {
		comp, exists := cfg.Components[req.instance]
		if !exists || !comp.Enabled || comp.Name != req.factory {
			return nil
		}
	}
	defaultCfg, err := config.NewLoader().LoadFromBytes(configs.DefaultFlowJSON)
	if err != nil {
		return fmt.Errorf("parse embedded default config: %w", err)
	}
	mavComp, ok := defaultCfg.Components[MAVLinkIngestComponent]
	if !ok {
		return fmt.Errorf("embedded default config missing %q", MAVLinkIngestComponent)
	}
	if cfg.Components == nil {
		cfg.Components = make(config.ComponentConfigs)
	}
	cfg.Components[MAVLinkIngestComponent] = mavComp
	return nil
}

// PrepareConfig reads path once, validates the un-overridden base flow that
// ConfigManager persists to KV, applies listener overrides and optional server
// version onto a deep clone, and validates the resulting effective flow offline.
func PrepareConfig(path string, overrides PortOverrides, version string) (*PreparedConfig, error) {
	baseCfg, err := LoadConfig(path, PortOverrides{})
	if err != nil {
		return nil, err
	}
	if _, err := ValidateOffline(baseCfg); err != nil {
		return nil, err
	}

	effectiveCfg := baseCfg.Clone()
	if err := ApplyPortOverrides(effectiveCfg, overrides); err != nil {
		return nil, err
	}
	if version != "" {
		if err := setServerVersion(effectiveCfg, version); err != nil {
			return nil, err
		}
	}
	compRes, err := ValidateOffline(effectiveCfg)
	if err != nil {
		return nil, err
	}
	return &PreparedConfig{
		Base:        baseCfg,
		Effective:   effectiveCfg,
		Composition: compRes,
	}, nil
}

// ApplyPortOverrides mutates cfg with validated listener overrides.
func ApplyPortOverrides(cfg *config.Config, overrides PortOverrides) error {
	if cfg == nil {
		return errors.New("host: nil configuration")
	}
	if cfg.Services == nil {
		cfg.Services = make(types.ServiceConfigs)
	}

	if overrides.HTTPPort != 0 {
		if err := validateUserPort("http-port", overrides.HTTPPort); err != nil {
			return err
		}
		if err := patchJSONField(cfg.Services, "service-manager", "http_port", overrides.HTTPPort, false); err != nil {
			return err
		}
	}

	if overrides.MetricsPort < 0 {
		entry := cfg.Services["metrics"]
		entry.Enabled = false
		if isEmptyOrNullJSON(entry.Config) {
			entry.Config = json.RawMessage(`{"port":9090,"path":"/metrics"}`)
		}
		cfg.Services["metrics"] = entry
	} else if overrides.MetricsPort > 0 {
		if err := validateUserPort("metrics-port", overrides.MetricsPort); err != nil {
			return err
		}
		if err := patchJSONField(cfg.Services, "metrics", "port", overrides.MetricsPort, true); err != nil {
			return err
		}
	}

	if overrides.TelemetryWSPort != 0 || overrides.BindHost != "" {
		if overrides.TelemetryWSPort != 0 {
			if err := validateUserPort("telemetry-ws-port", overrides.TelemetryWSPort); err != nil {
				return err
			}
		}
		if err := patchWebSocketEndpoint(cfg, TelemetryWSComponent, overrides.BindHost, overrides.TelemetryWSPort); err != nil {
			return err
		}
	}

	if overrides.GraphWSPort != 0 || overrides.BindHost != "" {
		if overrides.GraphWSPort != 0 {
			if err := validateUserPort("graph-ws-port", overrides.GraphWSPort); err != nil {
				return err
			}
		}
		if err := patchWebSocketEndpoint(cfg, GraphWSComponent, overrides.BindHost, overrides.GraphWSPort); err != nil {
			return err
		}
	}

	if overrides.MAVLinkUDPPort < 0 {
		comp, ok := cfg.Components[MAVLinkIngestComponent]
		if !ok {
			return fmt.Errorf("missing required component %q", MAVLinkIngestComponent)
		}
		comp.Enabled = false
		cfg.Components[MAVLinkIngestComponent] = comp
	} else if overrides.MAVLinkUDPPort > 0 || overrides.BindHost != "" {
		if overrides.MAVLinkUDPPort > 0 {
			if err := validateUserPort("mavlink-udp-port", overrides.MAVLinkUDPPort); err != nil {
				return err
			}
		}
		if err := patchMAVLinkUDPEndpoint(cfg, overrides.BindHost, overrides.MAVLinkUDPPort); err != nil {
			return err
		}
	}

	return nil
}

// ValidateOffline validates the SemEngine config document, ENTITY stream retention
// invariants, mandatory SemEngine services, required bbops SKG/GraphQL/WebSocket/MAVLink
// components, listener port uniqueness, and offline component composition graph.
func ValidateOffline(cfg *config.Config) (*composition.Result, error) {
	if cfg == nil {
		return nil, errors.New("host: nil configuration")
	}
	if err := uxsvocab.Register(); err != nil {
		return nil, fmt.Errorf("register uxs vocabulary pack: %w", err)
	}
	if err := control.RegisterVocab(); err != nil {
		return nil, fmt.Errorf("register control vocabulary pack: %w", err)
	}
	entityStream, ok := cfg.Streams[EntityStreamName]
	if !ok {
		return nil, fmt.Errorf("missing required %s stream declaration", EntityStreamName)
	}
	if _, archival := cfg.ArchivalStreams[EntityStreamName]; archival {
		return nil, fmt.Errorf("%s stream cannot be archival; finite retention is required", EntityStreamName)
	}
	if !slices.Contains(entityStream.Subjects, "entity.>") {
		return nil, fmt.Errorf("%s stream subjects %v must include entity.>", EntityStreamName, entityStream.Subjects)
	}
	if !hasValidSemEngineMaxAge(entityStream.MaxAge) || entityStream.MaxBytes <= 0 || entityStream.Discard != config.StreamDiscardOld {
		return nil, fmt.Errorf("%s stream requires explicit finite max_age, max_bytes > 0, and discard \"old\"", EntityStreamName)
	}
	if _, err := config.ValidateStreamDeclarations(cfg); err != nil {
		return nil, fmt.Errorf("validate stream declarations: %w", err)
	}

	for _, req := range requiredM02Components {
		comp, exists := cfg.Components[req.instance]
		if !exists || !comp.Enabled {
			return nil, fmt.Errorf("required component %q is missing or disabled", req.instance)
		}
		if comp.Name != req.factory {
			return nil, fmt.Errorf("required component %q must use factory %q, got %q", req.instance, req.factory, comp.Name)
		}
	}

	mavComp, hasMAV := cfg.Components[MAVLinkIngestComponent]
	if !hasMAV {
		return nil, fmt.Errorf("required component %q is missing", MAVLinkIngestComponent)
	}
	if mavComp.Name != mavlink.FactoryName {
		return nil, fmt.Errorf("required component %q must use factory %q, got %q", MAVLinkIngestComponent, mavlink.FactoryName, mavComp.Name)
	}

	if _, err := ResolveEndpoints(cfg); err != nil {
		return nil, err
	}

	if err := validateServiceCompositionOffline(cfg); err != nil {
		return nil, err
	}

	catalog := component.NewRegistry()
	if err := RegisterComponents(catalog); err != nil {
		return nil, err
	}
	result, err := composition.Validate(catalog, cfg)
	if err != nil {
		return nil, fmt.Errorf("validate composition: %w", err)
	}
	if result.Status == composition.StatusErrors || len(result.Errors) > 0 {
		msgs := make([]string, 0, len(result.Errors))
		for _, f := range result.Errors {
			msgs = append(msgs, fmt.Sprintf("%s (%s): %s", f.Component, f.Type, f.Message))
		}
		return result, fmt.Errorf("composition validation failed: %s", strings.Join(msgs, "; "))
	}

	enabledCount := 0
	for _, entry := range cfg.Components {
		if entry.Enabled {
			enabledCount++
		}
	}
	if enabledCount == 0 {
		return result, errors.New("composition enables zero components")
	}
	if len(result.Graph.Nodes) != enabledCount {
		return result, fmt.Errorf("composition admitted %d/%d enabled components", len(result.Graph.Nodes), enabledCount)
	}
	return result, nil
}

func validateServiceCompositionOffline(cfg *config.Config) error {
	discardLogger := slog.New(slog.DiscardHandler)
	offlineClient, err := natsclient.NewClient("nats://127.0.0.1:4222", natsclient.WithLogger(discardLogger))
	if err != nil {
		return fmt.Errorf("create offline NATS client: %w", err)
	}
	cfgMgr, err := config.NewConfigManager(cfg, offlineClient, discardLogger)
	if err != nil {
		return fmt.Errorf("validate platform config: %w", err)
	}
	compReg := component.NewRegistry()
	if err := RegisterComponents(compReg); err != nil {
		return err
	}
	svcReg := service.NewServiceRegistry()
	if err := service.RegisterAll(svcReg); err != nil {
		return fmt.Errorf("register framework services: %w", err)
	}
	svcMgr := service.NewServiceManager(svcReg)
	svcDeps := &service.Dependencies{
		NATSClient:        offlineClient,
		MetricsRegistry:   metric.NewMetricsRegistry(),
		Logger:            discardLogger,
		Platform:          types.PlatformMeta{Org: cfg.Platform.Org, Platform: cfg.Platform.ID},
		Manager:           cfgMgr,
		ComponentRegistry: compReg,
		PayloadRegistry:   payloadregistry.New(),
	}
	if err := svcMgr.ConfigureFromServices(cfg.Services, svcDeps); err != nil {
		return fmt.Errorf("configure services: %w", err)
	}
	return nil
}

// ResolveEndpoints extracts the effective HTTP, WebSocket, and MAVLink UDP endpoints
// from cfg and verifies that no two active TCP listeners claim the same port.
func ResolveEndpoints(cfg *config.Config) (ResolvedEndpoints, error) {
	if cfg == nil {
		return ResolvedEndpoints{}, errors.New("host: nil configuration")
	}
	resolvedServices, err := service.ResolveServiceConfigs(cfg.Services)
	if err != nil {
		return ResolvedEndpoints{}, fmt.Errorf("resolve service configs: %w", err)
	}

	httpPort := 8080
	if svc, ok := resolvedServices["service-manager"]; ok && !isEmptyOrNullJSON(svc.Config) {
		var parsed struct {
			HTTPPort int `json:"http_port"`
		}
		if err := json.Unmarshal(svc.Config, &parsed); err != nil {
			return ResolvedEndpoints{}, fmt.Errorf("decode service-manager config: %w", err)
		}
		if parsed.HTTPPort != 0 {
			httpPort = parsed.HTTPPort
		}
	}
	if err := validateUserPort("service-manager.http_port", httpPort); err != nil {
		return ResolvedEndpoints{}, err
	}

	portsUsed := map[int]string{httpPort: "service-manager"}

	metricsPort := 9090
	metricsSvc := resolvedServices["metrics"]
	metricsEnabled := metricsSvc.Enabled
	if !isEmptyOrNullJSON(metricsSvc.Config) {
		var parsed struct {
			Port int `json:"port"`
		}
		if err := json.Unmarshal(metricsSvc.Config, &parsed); err != nil {
			return ResolvedEndpoints{}, fmt.Errorf("decode metrics config: %w", err)
		}
		if parsed.Port != 0 {
			metricsPort = parsed.Port
		}
	}
	if metricsEnabled {
		if err := validateUserPort("metrics.port", metricsPort); err != nil {
			return ResolvedEndpoints{}, err
		}
		if holder, clash := portsUsed[metricsPort]; clash {
			return ResolvedEndpoints{}, fmt.Errorf("port %d claimed by both %s and metrics", metricsPort, holder)
		}
		portsUsed[metricsPort] = "metrics"
	} else {
		metricsPort = 0
	}

	telemetryURL, telemetryPort, err := extractWebSocketURL(cfg, TelemetryWSComponent)
	if err != nil {
		return ResolvedEndpoints{}, err
	}
	if holder, clash := portsUsed[telemetryPort]; clash {
		return ResolvedEndpoints{}, fmt.Errorf("port %d claimed by both %s and %s", telemetryPort, holder, TelemetryWSComponent)
	}
	portsUsed[telemetryPort] = TelemetryWSComponent

	graphURL, graphPort, err := extractWebSocketURL(cfg, GraphWSComponent)
	if err != nil {
		return ResolvedEndpoints{}, err
	}
	if holder, clash := portsUsed[graphPort]; clash {
		return ResolvedEndpoints{}, fmt.Errorf("port %d claimed by both %s and %s", graphPort, holder, GraphWSComponent)
	}

	mavlinkAddr, mavlinkPort, err := extractMAVLinkUDPEndpoint(cfg)
	if err != nil {
		return ResolvedEndpoints{}, err
	}

	return ResolvedEndpoints{
		HTTPPort:       httpPort,
		MetricsPort:    metricsPort,
		MAVLinkUDPPort: mavlinkPort,
		HTTPBaseURL:    fmt.Sprintf("http://127.0.0.1:%d", httpPort),
		TelemetryWS:    telemetryURL,
		GraphWS:        graphURL,
		MAVLinkUDPAddr: mavlinkAddr,
	}, nil
}

func validateUserPort(name string, port int) error {
	if port < minUserPort || port > maxUserPort {
		return fmt.Errorf("%s %d out of range %d..%d", name, port, minUserPort, maxUserPort)
	}
	return nil
}

func hasValidSemEngineMaxAge(raw string) bool {
	if raw == "" {
		return false
	}
	if strings.HasSuffix(raw, "d") {
		n, err := strconv.Atoi(strings.TrimSuffix(raw, "d"))
		maxDays := int(math.MaxInt64 / int64(24*time.Hour))
		return err == nil && n > 0 && n <= maxDays
	}
	d, err := time.ParseDuration(raw)
	return err == nil && d > 0
}

func isEmptyOrNullJSON(raw json.RawMessage) bool {
	trimmed := bytes.TrimSpace(raw)
	return len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null"))
}

func patchJSONField(services types.ServiceConfigs, serviceName, key string, value any, forceEnable bool) error {
	entry, exists := services[serviceName]
	raw := map[string]any{}
	if exists && !isEmptyOrNullJSON(entry.Config) {
		if err := json.Unmarshal(entry.Config, &raw); err != nil {
			return fmt.Errorf("decode %s config: %w", serviceName, err)
		}
		if raw == nil {
			raw = map[string]any{}
		}
	}
	raw[key] = value
	encoded, err := json.Marshal(raw)
	if err != nil {
		return fmt.Errorf("encode %s config: %w", serviceName, err)
	}
	if !exists || forceEnable {
		entry.Enabled = true
	}
	entry.Config = encoded
	services[serviceName] = entry
	return nil
}

func patchWebSocketEndpoint(cfg *config.Config, componentName, host string, port int) error {
	comp, ok := cfg.Components[componentName]
	if !ok {
		return fmt.Errorf("missing required component %q", componentName)
	}
	if isEmptyOrNullJSON(comp.Config) {
		return fmt.Errorf("%s config missing ports block", componentName)
	}
	var doc map[string]any
	if err := json.Unmarshal(comp.Config, &doc); err != nil {
		return fmt.Errorf("decode %s config: %w", componentName, err)
	}
	portsMap, _ := doc["ports"].(map[string]any)
	if portsMap == nil {
		return fmt.Errorf("%s config missing ports block", componentName)
	}
	outputs, _ := portsMap["outputs"].([]any)
	if len(outputs) != 1 {
		return fmt.Errorf("%s expected 1 output port, got %d", componentName, len(outputs))
	}
	outDef, _ := outputs[0].(map[string]any)
	outCfg, _ := outDef["config"].(map[string]any)
	if outCfg == nil {
		return fmt.Errorf("%s output port missing config object", componentName)
	}
	if host != "" {
		outCfg["host"] = host
	}
	if port != 0 {
		outCfg["port"] = port
	}
	encoded, err := json.Marshal(doc)
	if err != nil {
		return fmt.Errorf("encode %s config: %w", componentName, err)
	}
	comp.Config = encoded
	cfg.Components[componentName] = comp
	return nil
}

func patchMAVLinkUDPEndpoint(cfg *config.Config, host string, port int) error {
	comp, ok := cfg.Components[MAVLinkIngestComponent]
	if !ok {
		return fmt.Errorf("missing required component %q", MAVLinkIngestComponent)
	}
	if !comp.Enabled {
		return nil
	}
	if isEmptyOrNullJSON(comp.Config) {
		return fmt.Errorf("%s config missing ports block", MAVLinkIngestComponent)
	}
	var doc map[string]any
	if err := json.Unmarshal(comp.Config, &doc); err != nil {
		return fmt.Errorf("decode %s config: %w", MAVLinkIngestComponent, err)
	}
	portsMap, _ := doc["ports"].(map[string]any)
	if portsMap == nil {
		return fmt.Errorf("%s config missing ports block", MAVLinkIngestComponent)
	}
	inputs, _ := portsMap["inputs"].([]any)
	if len(inputs) != 1 {
		return fmt.Errorf("%s expected 1 input port, got %d", MAVLinkIngestComponent, len(inputs))
	}
	inDef, _ := inputs[0].(map[string]any)
	inCfg, _ := inDef["config"].(map[string]any)
	if inCfg == nil {
		return fmt.Errorf("%s input port missing config object", MAVLinkIngestComponent)
	}
	if host != "" {
		inCfg["host"] = host
	}
	if port > 0 {
		inCfg["port"] = port
	}
	encoded, err := json.Marshal(doc)
	if err != nil {
		return fmt.Errorf("encode %s config: %w", MAVLinkIngestComponent, err)
	}
	comp.Config = encoded
	cfg.Components[MAVLinkIngestComponent] = comp
	return nil
}

func extractWebSocketURL(cfg *config.Config, componentName string) (string, int, error) {
	comp, ok := cfg.Components[componentName]
	if !ok || !comp.Enabled {
		return "", 0, fmt.Errorf("required component %q is missing or disabled", componentName)
	}
	var parsed struct {
		Path  string `json:"path"`
		Ports struct {
			Outputs []struct {
				Config struct {
					Host string `json:"host"`
					Port int    `json:"port"`
				} `json:"config"`
			} `json:"outputs"`
		} `json:"ports"`
	}
	if err := json.Unmarshal(comp.Config, &parsed); err != nil {
		return "", 0, fmt.Errorf("decode %s config: %w", componentName, err)
	}
	if len(parsed.Ports.Outputs) != 1 {
		return "", 0, fmt.Errorf("%s must declare exactly 1 output port", componentName)
	}
	wsPath := parsed.Path
	if wsPath == "" {
		wsPath = "/ws"
	}
	port := parsed.Ports.Outputs[0].Config.Port
	if err := validateUserPort(componentName+".port", port); err != nil {
		return "", 0, err
	}
	dialHost := parsed.Ports.Outputs[0].Config.Host
	if dialHost == "" || dialHost == "0.0.0.0" || dialHost == "::" {
		dialHost = "127.0.0.1"
	}
	return "ws://" + net.JoinHostPort(dialHost, strconv.Itoa(port)) + wsPath, port, nil
}

func extractMAVLinkUDPEndpoint(cfg *config.Config) (string, int, error) {
	comp, ok := cfg.Components[MAVLinkIngestComponent]
	if !ok || !comp.Enabled {
		return "", 0, nil
	}
	var parsed struct {
		Ports struct {
			Inputs []struct {
				Config struct {
					Protocol string `json:"protocol"`
					Host     string `json:"host"`
					Port     int    `json:"port"`
				} `json:"config"`
			} `json:"inputs"`
		} `json:"ports"`
	}
	if err := json.Unmarshal(comp.Config, &parsed); err != nil {
		return "", 0, fmt.Errorf("decode %s config: %w", MAVLinkIngestComponent, err)
	}
	if len(parsed.Ports.Inputs) != 1 {
		return "", 0, fmt.Errorf("%s must declare exactly 1 input port", MAVLinkIngestComponent)
	}
	inCfg := parsed.Ports.Inputs[0].Config
	if !strings.EqualFold(inCfg.Protocol, "udp") {
		return "", 0, fmt.Errorf("%s input port protocol must be \"udp\", got %q", MAVLinkIngestComponent, inCfg.Protocol)
	}
	if err := validateUserPort(MAVLinkIngestComponent+".port", inCfg.Port); err != nil {
		return "", 0, err
	}
	dialHost := inCfg.Host
	if dialHost == "" || dialHost == "0.0.0.0" || dialHost == "::" {
		dialHost = "127.0.0.1"
	}
	return net.JoinHostPort(dialHost, strconv.Itoa(inCfg.Port)), inCfg.Port, nil
}

// RedactNATSURL strips userinfo, query parameters, fragments, and paths from one
// or more comma-separated NATS server URLs, preserving only scheme, hostname,
// and port (defaulting schemeless addresses to "nats"). Unparseable segments
// return "<redacted>" so credentials never leak through malformed URLs.
func RedactNATSURL(raw string) string {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return ""
	}
	parts := strings.Split(trimmed, ",")
	redacted := make([]string, 0, len(parts))
	for _, part := range parts {
		seg := strings.TrimSpace(part)
		if seg == "" {
			continue
		}
		candidate := seg
		if !strings.Contains(candidate, "://") {
			candidate = "nats://" + candidate
		}
		u, err := url.Parse(candidate)
		if err != nil || u.Scheme == "" || u.Hostname() == "" {
			redacted = append(redacted, "<redacted>")
			continue
		}
		host := u.Hostname()
		if port := u.Port(); port != "" {
			host = net.JoinHostPort(host, port)
		} else if strings.Contains(host, ":") {
			host = "[" + host + "]"
		}
		redacted = append(redacted, strings.ToLower(u.Scheme)+"://"+host)
	}
	return strings.Join(redacted, ",")
}

func extractNATSURLSecrets(raw string) []string {
	var secrets []string
	addSecret := func(s string) {
		if s != "" && !slices.Contains(secrets, s) {
			secrets = append(secrets, s)
		}
	}
	for _, part := range strings.Split(strings.TrimSpace(raw), ",") {
		seg := strings.TrimSpace(part)
		if seg == "" {
			continue
		}
		candidate := seg
		if !strings.Contains(candidate, "://") {
			candidate = "nats://" + candidate
		}
		if u, err := url.Parse(candidate); err == nil && u.User != nil {
			addSecret(u.User.String())
			addSecret(u.User.Username())
			if pass, ok := u.User.Password(); ok {
				addSecret(pass)
			}
			addSecret(seg)
			continue
		}
		if strings.Contains(seg, "@") {
			addSecret(seg)
		}
	}
	return secrets
}
