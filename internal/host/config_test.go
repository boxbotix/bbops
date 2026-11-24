package host

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/boxbotix/bbops/configs"
	"github.com/c360studio/semengine/composition"
)

// spec: host-runtime / Host CLI version and flag entrypoint
// spec: host-runtime / Embedded NATS JetStream and SemEngine SKG pipeline lifecycle
// spec: host-runtime / ADR-115 GraphQL front door and dual output/websocket streaming planes
// spec: mavlink-ingest / Three-lane hybrid MAVLink v2 UDP telemetry and SKG ingest component
func TestValidateOfflineAndPortOverrides(t *testing.T) {
	t.Run("embedded default flow validates all eight components, flow paths, and endpoints", func(t *testing.T) {
		cfg, err := LoadConfig("", PortOverrides{})
		if err != nil {
			t.Fatalf("LoadConfig default: %v", err)
		}
		res, err := ValidateOffline(cfg)
		if err != nil {
			t.Fatalf("ValidateOffline default: %v", err)
		}
		if got, want := len(res.Graph.Nodes), 8; got != want {
			t.Fatalf("admitted component nodes = %d, want %d", got, want)
		}
		paths := composition.FlowPaths(res.Graph)[MAVLinkIngestComponent]
		if !slices.Contains(paths, TelemetryWSComponent) || !slices.Contains(paths, "graph-ingest") {
			t.Fatalf("mavlink-ingest flow paths = %v, want reachability to %s and graph-ingest", paths, TelemetryWSComponent)
		}
		endpoints, err := ResolveEndpoints(cfg)
		if err != nil {
			t.Fatalf("ResolveEndpoints default: %v", err)
		}
		if endpoints.HTTPBaseURL != "http://127.0.0.1:8080" {
			t.Fatalf("HTTPBaseURL = %q, want http://127.0.0.1:8080", endpoints.HTTPBaseURL)
		}
		if endpoints.TelemetryWS != "ws://127.0.0.1:8081/ws" {
			t.Fatalf("TelemetryWS = %q, want ws://127.0.0.1:8081/ws", endpoints.TelemetryWS)
		}
		if endpoints.GraphWS != "ws://127.0.0.1:8082/ws" {
			t.Fatalf("GraphWS = %q, want ws://127.0.0.1:8082/ws", endpoints.GraphWS)
		}
		if endpoints.MetricsPort != 9090 {
			t.Fatalf("MetricsPort = %d, want 9090", endpoints.MetricsPort)
		}
		if endpoints.MAVLinkUDPPort != 14550 || endpoints.MAVLinkUDPAddr != "127.0.0.1:14550" {
			t.Fatalf("MAVLinkUDP = (%d, %q), want (14550, 127.0.0.1:14550)", endpoints.MAVLinkUDPPort, endpoints.MAVLinkUDPAddr)
		}
	})

	t.Run("ENTITY stream accepts SemEngine day durations and rejects missing or non-finite retention", func(t *testing.T) {
		for _, validMaxAge := range []string{"24h", "1d", "14d"} {
			cfg, err := LoadConfig("", PortOverrides{})
			if err != nil {
				t.Fatalf("LoadConfig(%q): %v", validMaxAge, err)
			}
			stream := cfg.Streams[EntityStreamName]
			stream.MaxAge = validMaxAge
			cfg.Streams[EntityStreamName] = stream
			if _, err := ValidateOffline(cfg); err != nil {
				t.Fatalf("ValidateOffline(max_age=%q) unexpected error: %v", validMaxAge, err)
			}
		}

		for _, invalidMaxAge := range []string{"", "0s", "0h", "0d", "-1h", "-1d", "1.5d", "bogus", "999999d"} {
			cfg, err := LoadConfig("", PortOverrides{})
			if err != nil {
				t.Fatalf("LoadConfig(%q): %v", invalidMaxAge, err)
			}
			stream := cfg.Streams[EntityStreamName]
			stream.MaxAge = invalidMaxAge
			cfg.Streams[EntityStreamName] = stream
			if _, err := ValidateOffline(cfg); err == nil {
				t.Fatalf("expected ValidateOffline error for ENTITY max_age=%q", invalidMaxAge)
			}
		}

		cfgArchival, err := LoadConfig("", PortOverrides{})
		if err != nil {
			t.Fatalf("LoadConfig archival: %v", err)
		}
		if err := json.Unmarshal([]byte(`{"ENTITY":{"reason":"test"}}`), &cfgArchival.ArchivalStreams); err != nil {
			t.Fatalf("unmarshal archival_streams: %v", err)
		}
		if _, err := ValidateOffline(cfgArchival); err == nil || !strings.Contains(err.Error(), "cannot be archival") {
			t.Fatalf("expected archival ENTITY stream rejection, got %v", err)
		}

		cfg, err := LoadConfig("", PortOverrides{})
		if err != nil {
			t.Fatalf("LoadConfig: %v", err)
		}
		stream := cfg.Streams[EntityStreamName]
		stream.MaxBytes = 0
		cfg.Streams[EntityStreamName] = stream
		if _, err := ValidateOffline(cfg); err == nil {
			t.Fatal("expected ValidateOffline error when ENTITY max_bytes is 0")
		}

		cfg2, err := LoadConfig("", PortOverrides{})
		if err != nil {
			t.Fatalf("LoadConfig: %v", err)
		}
		stream2 := cfg2.Streams[EntityStreamName]
		stream2.Discard = "new"
		cfg2.Streams[EntityStreamName] = stream2
		if _, err := ValidateOffline(cfg2); err == nil {
			t.Fatal("expected ValidateOffline error when ENTITY discard != old")
		}

		cfg3, err := LoadConfig("", PortOverrides{})
		if err != nil {
			t.Fatalf("LoadConfig: %v", err)
		}
		delete(cfg3.Streams, EntityStreamName)
		if _, err := ValidateOffline(cfg3); err == nil {
			t.Fatal("expected ValidateOffline error when ENTITY stream is missing")
		}
	})

	t.Run("PrepareConfig rejects invalid base config even when port override masks collision", func(t *testing.T) {
		var doc map[string]any
		if err := json.Unmarshal(configs.DefaultFlowJSON, &doc); err != nil {
			t.Fatalf("unmarshal default flow: %v", err)
		}
		services := doc["services"].(map[string]any)
		metrics := services["metrics"].(map[string]any)
		metricsCfg := metrics["config"].(map[string]any)
		metricsCfg["port"] = 8080
		encoded, err := json.Marshal(doc)
		if err != nil {
			t.Fatalf("marshal colliding flow: %v", err)
		}
		cfgPath := filepath.Join(t.TempDir(), "masked-collision.json")
		if err := os.WriteFile(cfgPath, encoded, 0o600); err != nil {
			t.Fatalf("write colliding flow: %v", err)
		}

		if _, err := PrepareConfig(cfgPath, PortOverrides{MetricsPort: 19090}, "v0.2.0"); err == nil ||
			!strings.Contains(err.Error(), "port 8080 claimed by both service-manager and metrics") {
			t.Fatalf("expected PrepareConfig to reject colliding base config despite override, got %v", err)
		}

		prepared, err := PrepareConfig("", PortOverrides{HTTPPort: 18080, MetricsPort: 19090}, "v0.2.0-prep")
		if err != nil {
			t.Fatalf("PrepareConfig default with overrides: %v", err)
		}
		baseEP, err := ResolveEndpoints(prepared.Base)
		if err != nil {
			t.Fatalf("ResolveEndpoints(Base): %v", err)
		}
		effEP, err := ResolveEndpoints(prepared.Effective)
		if err != nil {
			t.Fatalf("ResolveEndpoints(Effective): %v", err)
		}
		if baseEP.HTTPPort != 8080 || baseEP.MetricsPort != 9090 {
			t.Fatalf("Base endpoints mutated by overrides: %+v", baseEP)
		}
		if effEP.HTTPPort != 18080 || effEP.MetricsPort != 19090 {
			t.Fatalf("Effective endpoints = %+v, want HTTPPort=18080 MetricsPort=19090", effEP)
		}
	})

	t.Run("mandatory SemEngine services cannot be disabled offline", func(t *testing.T) {
		cfgCompMgr, err := LoadConfig("", PortOverrides{})
		if err != nil {
			t.Fatalf("LoadConfig: %v", err)
		}
		svc := cfgCompMgr.Services["component-manager"]
		svc.Enabled = false
		cfgCompMgr.Services["component-manager"] = svc
		if _, err := ValidateOffline(cfgCompMgr); err == nil || !strings.Contains(err.Error(), "mandatory service component-manager cannot be disabled") {
			t.Fatalf("expected mandatory component-manager error, got %v", err)
		}

		cfgSvcMgr, err := LoadConfig("", PortOverrides{})
		if err != nil {
			t.Fatalf("LoadConfig: %v", err)
		}
		sm := cfgSvcMgr.Services["service-manager"]
		sm.Enabled = false
		cfgSvcMgr.Services["service-manager"] = sm
		// Even with an explicit HTTPPort override, service-manager.enabled=false must stay disabled and fail.
		if err := ApplyPortOverrides(cfgSvcMgr, PortOverrides{HTTPPort: 18080}); err != nil {
			t.Fatalf("ApplyPortOverrides: %v", err)
		}
		if _, err := ValidateOffline(cfgSvcMgr); err == nil || !strings.Contains(err.Error(), "mandatory service service-manager cannot be disabled") {
			t.Fatalf("expected mandatory service-manager error, got %v", err)
		}
	})

	t.Run("required M0.2 SKG pipeline and GraphQL front door cannot be disabled", func(t *testing.T) {
		for _, required := range []string{
			"graph-ingest",
			"graph-index",
			"graph-lexical",
			"graph-query",
			"graph-gateway",
			TelemetryWSComponent,
			GraphWSComponent,
		} {
			cfg, err := LoadConfig("", PortOverrides{})
			if err != nil {
				t.Fatalf("LoadConfig: %v", err)
			}
			comp := cfg.Components[required]
			comp.Enabled = false
			cfg.Components[required] = comp
			if _, err := ValidateOffline(cfg); err == nil || !strings.Contains(err.Error(), required) {
				t.Fatalf("expected ValidateOffline error naming disabled required component %q, got %v", required, err)
			}
		}
	})

	t.Run("port overrides enforce 1024..65535 range and reject port collisions", func(t *testing.T) {
		if _, err := LoadConfig("", PortOverrides{HTTPPort: 1023}); err == nil {
			t.Fatal("expected error for HTTPPort < 1024")
		}
		if _, err := LoadConfig("", PortOverrides{TelemetryWSPort: 65536}); err == nil {
			t.Fatal("expected error for TelemetryWSPort > 65535")
		}
		if _, err := LoadConfig("", PortOverrides{MAVLinkUDPPort: 1023}); err == nil {
			t.Fatal("expected error for MAVLinkUDPPort < 1024")
		}
		if _, err := LoadConfig("", PortOverrides{MAVLinkUDPPort: 65536}); err == nil {
			t.Fatal("expected error for MAVLinkUDPPort > 65535")
		}

		colliding, err := LoadConfig("", PortOverrides{
			HTTPPort:        18080,
			TelemetryWSPort: 18080,
		})
		if err != nil {
			t.Fatalf("LoadConfig colliding: %v", err)
		}
		if _, err := ValidateOffline(colliding); err == nil || !strings.Contains(err.Error(), "claimed by both") {
			t.Fatalf("expected port collision error, got %v", err)
		}

		valid, err := LoadConfig("", PortOverrides{
			HTTPPort:        18080,
			TelemetryWSPort: 18081,
			GraphWSPort:     18082,
			MetricsPort:     -1,
			MAVLinkUDPPort:  14555,
			BindHost:        "127.0.0.1",
		})
		if err != nil {
			t.Fatalf("LoadConfig valid overrides: %v", err)
		}
		endpoints, err := ResolveEndpoints(valid)
		if err != nil {
			t.Fatalf("ResolveEndpoints valid overrides: %v", err)
		}
		if endpoints.MetricsPort != 0 {
			t.Fatalf("MetricsPort = %d, want 0 when disabled", endpoints.MetricsPort)
		}
		if endpoints.TelemetryWS != "ws://127.0.0.1:18081/ws" || endpoints.GraphWS != "ws://127.0.0.1:18082/ws" {
			t.Fatalf("unexpected WS endpoints: %+v", endpoints)
		}
		if endpoints.MAVLinkUDPPort != 14555 || endpoints.MAVLinkUDPAddr != "127.0.0.1:14555" {
			t.Fatalf("unexpected MAVLinkUDP endpoints: %+v", endpoints)
		}

		disabledUDP, err := LoadConfig("", PortOverrides{MAVLinkUDPPort: -1})
		if err != nil {
			t.Fatalf("LoadConfig MAVLinkUDPPort=-1: %v", err)
		}
		resDisabled, err := ValidateOffline(disabledUDP)
		if err != nil {
			t.Fatalf("ValidateOffline MAVLinkUDPPort=-1: %v", err)
		}
		if got, want := len(resDisabled.Graph.Nodes), 7; got != want {
			t.Fatalf("admitted nodes with MAVLinkUDPPort=-1 = %d, want %d", got, want)
		}
		epDisabled, err := ResolveEndpoints(disabledUDP)
		if err != nil {
			t.Fatalf("ResolveEndpoints MAVLinkUDPPort=-1: %v", err)
		}
		if epDisabled.MAVLinkUDPPort != 0 || epDisabled.MAVLinkUDPAddr != "" {
			t.Fatalf("disabled MAVLinkUDP endpoints = (%d, %q), want (0, empty)", epDisabled.MAVLinkUDPPort, epDisabled.MAVLinkUDPAddr)
		}

		wrongFactory, err := LoadConfig("", PortOverrides{})
		if err != nil {
			t.Fatalf("LoadConfig wrong factory: %v", err)
		}
		mv := wrongFactory.Components[MAVLinkIngestComponent]
		mv.Name = "websocket"
		wrongFactory.Components[MAVLinkIngestComponent] = mv
		if _, err := ValidateOffline(wrongFactory); err == nil || !strings.Contains(err.Error(), MAVLinkIngestComponent) {
			t.Fatalf("expected wrong mavlink-ingest factory error, got %v", err)
		}
	})
}

// spec: host-runtime / ADR-115 GraphQL front door and dual output/websocket streaming planes
func TestStaticUIMiddleware(t *testing.T) {
	uiDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(uiDir, "index.html"), []byte("<html>bbops-cop</html>"), 0o600); err != nil {
		t.Fatalf("write index.html: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(uiDir, "assets"), 0o750); err != nil {
		t.Fatalf("mkdir assets: %v", err)
	}
	if err := os.WriteFile(filepath.Join(uiDir, "assets", "app.js"), []byte("console.log('cop');"), 0o600); err != nil {
		t.Fatalf("write app.js: %v", err)
	}
	// Even if files matching reserved routes exist in uiDir, reserved routes must never be shadowed.
	if err := os.MkdirAll(filepath.Join(uiDir, "services"), 0o750); err != nil {
		t.Fatalf("mkdir services decoy: %v", err)
	}
	if err := os.WriteFile(filepath.Join(uiDir, "readyz"), []byte("shadowed"), 0o600); err != nil {
		t.Fatalf("write readyz decoy: %v", err)
	}
	if err := os.WriteFile(filepath.Join(uiDir, "services", "health"), []byte("shadowed-services-health"), 0o600); err != nil {
		t.Fatalf("write services/health decoy: %v", err)
	}

	var ready atomic.Bool
	// Simulate SemEngine's post-commit mux where "/" is unregistered (404) while reserved routes exist.
	backend := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" || strings.HasPrefix(r.URL.Path, "/missions/") {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("X-Backend-Route", r.URL.Path)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("BACKEND:" + r.URL.Path))
	})
	handler := NewStaticUIMiddleware(uiDir, ready.Load)(backend)

	// 1. Before readiness, GET / answers 503 NOT READY (never 200 SPA or 404) while reserved routes still pass through.
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusServiceUnavailable || rec.Body.String() != "NOT READY" {
		t.Fatalf("pre-ready GET / = %d %q, want 503 NOT READY", rec.Code, rec.Body.String())
	}
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/services/health", nil))
	if got, want := rec.Body.String(), "BACKEND:/services/health"; got != want {
		t.Fatalf("pre-ready GET /services/health = %q, want %q", got, want)
	}

	ready.Store(true)

	// 2. Post-ready GET / and SPA client routes serve index.html.
	for _, route := range []string{"/", "/missions/alpha-1"} {
		rec = httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, route, nil))
		if rec.Code != http.StatusOK || rec.Body.String() != "<html>bbops-cop</html>" {
			t.Fatalf("GET %s = %d %q, want 200 index.html", route, rec.Code, rec.Body.String())
		}
	}

	// 3. Static asset serves exact file bytes.
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/assets/app.js", nil))
	if rec.Code != http.StatusOK || rec.Body.String() != "console.log('cop');" {
		t.Fatalf("GET /assets/app.js = %d %q", rec.Code, rec.Body.String())
	}

	// 4. Reserved routes (including /services/health) and POST GraphQL requests bypass static UI unconditionally.
	for _, reserved := range []struct {
		method string
		path   string
	}{
		{http.MethodGet, "/readyz"},
		{http.MethodGet, "/healthz"},
		{http.MethodGet, "/health"},
		{http.MethodGet, "/services"},
		{http.MethodGet, "/services/health"},
		{http.MethodGet, "/components/list"},
		{http.MethodGet, "/graph/triples"},
		{http.MethodPost, "/graph-gateway/graphql"},
	} {
		rec = httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(reserved.method, reserved.path, nil))
		if got, want := rec.Body.String(), "BACKEND:"+reserved.path; got != want {
			t.Fatalf("%s %s = %q, want %q", reserved.method, reserved.path, got, want)
		}
	}
}

// spec: host-runtime / Host CLI version and flag entrypoint
// spec: host-runtime / Embedded NATS JetStream and SemEngine SKG pipeline lifecycle
func TestRedactNATSURL(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want string
	}{
		{
			name: "plain loopback URL unchanged",
			raw:  "nats://127.0.0.1:4222",
			want: "nats://127.0.0.1:4222",
		},
		{
			name: "token-only userinfo stripped",
			raw:  "nats://tok_fixture_secret_99@127.0.0.1:4222",
			want: "nats://127.0.0.1:4222",
		},
		{
			name: "username and password stripped",
			raw:  "nats://alice_fixture:s3cr3t_pass_fixture@127.0.0.1:4222/path?foo=bar#frag",
			want: "nats://127.0.0.1:4222",
		},
		{
			name: "schemeless token URL defaults to nats scheme without token",
			raw:  "tok_only_fixture@127.0.0.1:4222",
			want: "nats://127.0.0.1:4222",
		},
		{
			name: "comma-separated cluster URLs redact every member",
			raw:  "tls://tokA@n1.example.com:4222, tls://userB:passB@n2.example.com:4222",
			want: "tls://n1.example.com:4222,tls://n2.example.com:4222",
		},
		{
			name: "ipv6 host keeps brackets without credentials",
			raw:  "nats://tok_v6@[::1]:4222",
			want: "nats://[::1]:4222",
		},
		{
			name: "malformed URL with credentials returns redacted placeholder",
			raw:  "nats://bad_secret@:badport:extra",
			want: "<redacted>",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := RedactNATSURL(tc.raw)
			if got != tc.want {
				t.Fatalf("RedactNATSURL(%q) = %q, want %q", tc.raw, got, tc.want)
			}
		})
	}
}

// spec: host-runtime / ADR-115 GraphQL front door and dual output/websocket streaming planes
func FuzzStaticUIMiddlewareRouting(f *testing.F) {
	parentDir := f.TempDir()
	if err := os.WriteFile(filepath.Join(parentDir, "secret.txt"), []byte("TOP-SECRET-OUTSIDE-UIDIR"), 0o600); err != nil {
		f.Fatalf("write parent secret: %v", err)
	}
	uiDir := filepath.Join(parentDir, "build")
	for _, rel := range []string{"assets", "graph-gateway", "services"} {
		if err := os.MkdirAll(filepath.Join(uiDir, rel), 0o750); err != nil {
			f.Fatalf("mkdir %s: %v", rel, err)
		}
	}
	files := map[string]string{
		"index.html":            "<html>bbops-cop</html>",
		"assets/app.js":         "console.log('cop');",
		"readyz":                "DECOY-READYZ",
		"healthz":               "DECOY-HEALTHZ",
		"services/health":       "DECOY-SERVICES-HEALTH",
		"graph-gateway/graphql": "DECOY-GRAPHQL",
	}
	for rel, content := range files {
		if err := os.WriteFile(filepath.Join(uiDir, filepath.FromSlash(rel)), []byte(content), 0o600); err != nil {
			f.Fatalf("write %s: %v", rel, err)
		}
	}

	seeds := []struct {
		methodIdx uint8
		rawPath   string
		ready     bool
	}{
		{0, "/", false},
		{0, "/", true},
		{1, "/missions/usv-alpha", true},
		{0, "/assets/app.js", true},
		{0, "/readyz", true},
		{0, "/reAdYZ", true},
		{0, "/services/health", true},
		{0, "/services/health", false},
		{0, "/missions/../readyz", true},
		{2, "/graph-gateway/graphql", true},
		{0, "/graph-gateway/graphql", true},
		{0, "/components/graph-ingest", true},
		{0, "/../../secret.txt", true},
		{0, "/..\\secret.txt", true},
		{3, "/", true},
	}
	for _, s := range seeds {
		f.Add(s.methodIdx, s.rawPath, s.ready)
	}

	methods := []string{http.MethodGet, http.MethodHead, http.MethodPost, http.MethodDelete, http.MethodOptions}

	f.Fuzz(func(t *testing.T, methodIdx uint8, rawPath string, ready bool) {
		method := methods[int(methodIdx)%len(methods)]
		reqPath := "/" + strings.TrimLeft(rawPath, "/")

		var backendHits atomic.Int32
		backend := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			backendHits.Add(1)
			w.Header().Set("X-Backend-Hit", "1")
			w.WriteHeader(http.StatusAccepted)
			_, _ = w.Write([]byte("BACKEND-OWNED"))
		})

		handler := NewStaticUIMiddleware(uiDir, func() bool { return ready })(backend)
		rec := httptest.NewRecorder()
		req := &http.Request{
			Method:     method,
			URL:        &url.URL{Path: reqPath},
			Proto:      "HTTP/1.1",
			ProtoMajor: 1,
			ProtoMinor: 1,
			Header:     make(http.Header),
		}
		handler.ServeHTTP(rec, req)
		body := rec.Body.String()

		if strings.Contains(body, "TOP-SECRET-OUTSIDE-UIDIR") {
			t.Fatalf("path traversal leaked parent file for %s %q", method, reqPath)
		}
		if strings.Contains(body, "DECOY-") {
			t.Fatalf("reserved backend route shadowed by static decoy file for %s %q: %q", method, reqPath, body)
		}

		expectBackend := (method != http.MethodGet && method != http.MethodHead) ||
			referenceReservedBackendRoute(reqPath)

		if expectBackend {
			if backendHits.Load() != 1 || rec.Header().Get("X-Backend-Hit") != "1" {
				t.Fatalf("expected backend pass-through for %s %q (ready=%v), got code=%d body=%q", method, reqPath, ready, rec.Code, body)
			}
			return
		}

		if backendHits.Load() != 0 {
			t.Fatalf("expected middleware-handled UI response for %s %q (ready=%v), but backend was hit", method, reqPath, ready)
		}
		if !ready {
			if rec.Code != http.StatusServiceUnavailable || body != "NOT READY" {
				t.Fatalf("expected 503 NOT READY before readiness for %s %q, got %d %q", method, reqPath, rec.Code, body)
			}
		}
	})
}

// referenceReservedBackendRoute independently resolves dot-segments without calling
// path.Clean or isReservedBackendPath to serve as a test-owned oracle.
func referenceReservedBackendRoute(reqPath string) bool {
	var stack []string
	for _, seg := range strings.Split(reqPath, "/") {
		switch seg {
		case "", ".":
			continue
		case "..":
			if len(stack) > 0 {
				stack = stack[:len(stack)-1]
			}
		default:
			stack = append(stack, strings.ToLower(seg))
		}
	}
	if len(stack) == 0 {
		return false
	}
	switch stack[0] {
	case "health", "healthz", "readyz", "openapi.json":
		return len(stack) == 1
	case "services", "docs", "components", "graph", "graph-gateway", "message-logger", "storage-observability":
		return true
	default:
		return false
	}
}

// spec: host-runtime / Host CLI version and flag entrypoint
// spec: host-runtime / Embedded NATS JetStream and SemEngine SKG pipeline lifecycle
func TestNullServiceConfigsDoNotPanicOnOverridesOrVersion(t *testing.T) {
	var doc map[string]any
	if err := json.Unmarshal(configs.DefaultFlowJSON, &doc); err != nil {
		t.Fatalf("unmarshal default flow: %v", err)
	}
	servicesMap := doc["services"].(map[string]any)
	servicesMap["service-manager"] = map[string]any{
		"enabled": true,
		"config":  nil,
	}
	servicesMap["metrics"] = map[string]any{
		"enabled": true,
		"config":  nil,
	}
	rawJSON, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("marshal null-service flow: %v", err)
	}
	cfgPath := filepath.Join(t.TempDir(), "null-services.json")
	if err := os.WriteFile(cfgPath, rawJSON, 0o600); err != nil {
		t.Fatalf("write null-services.json: %v", err)
	}

	// 1. Port overrides on null service configs normalize cleanly without nil-map panic.
	overridden, err := LoadConfig(cfgPath, PortOverrides{
		HTTPPort:    18088,
		MetricsPort: 19098,
	})
	if err != nil {
		t.Fatalf("LoadConfig with overrides on null service configs: %v", err)
	}
	if _, err := ValidateOffline(overridden); err != nil {
		t.Fatalf("ValidateOffline overridden null service configs: %v", err)
	}
	ep, err := ResolveEndpoints(overridden)
	if err != nil {
		t.Fatalf("ResolveEndpoints overridden null service configs: %v", err)
	}
	if ep.HTTPPort != 18088 || ep.MetricsPort != 19098 {
		t.Fatalf("endpoints = %+v, want HTTPPort=18088 MetricsPort=19098", ep)
	}

	// 2. Disabling metrics on a null metrics config normalizes cleanly.
	disabledMetrics, err := LoadConfig(cfgPath, PortOverrides{MetricsPort: -1})
	if err != nil {
		t.Fatalf("LoadConfig MetricsPort=-1 on null service configs: %v", err)
	}
	if _, err := ValidateOffline(disabledMetrics); err != nil {
		t.Fatalf("ValidateOffline disabled metrics: %v", err)
	}

	// 3. Stamping server version onto a null service-manager config or null server_info does not panic.
	unoverridden, err := LoadConfig(cfgPath, PortOverrides{})
	if err != nil {
		t.Fatalf("LoadConfig unoverridden null service configs: %v", err)
	}
	if err := setServerVersion(unoverridden, "v0.2.9-nulltest"); err != nil {
		t.Fatalf("setServerVersion on null service-manager config: %v", err)
	}
	if _, err := ValidateOffline(unoverridden); err != nil {
		t.Fatalf("ValidateOffline after setServerVersion: %v", err)
	}
	var smCfg struct {
		ServerInfo struct {
			Version string `json:"version"`
		} `json:"server_info"`
	}
	if err := json.Unmarshal(unoverridden.Services["service-manager"].Config, &smCfg); err != nil {
		t.Fatalf("unmarshal stamped service-manager config: %v", err)
	}
	if smCfg.ServerInfo.Version != "v0.2.9-nulltest" {
		t.Fatalf("stamped version = %q, want v0.2.9-nulltest", smCfg.ServerInfo.Version)
	}

	smEntry := unoverridden.Services["service-manager"]
	smEntry.Config = json.RawMessage(`{"server_info":null}`)
	unoverridden.Services["service-manager"] = smEntry
	if err := setServerVersion(unoverridden, "v0.2.10-nullinfo"); err != nil {
		t.Fatalf("setServerVersion on null server_info: %v", err)
	}
	if err := json.Unmarshal(unoverridden.Services["service-manager"].Config, &smCfg); err != nil {
		t.Fatalf("unmarshal stamped null-info config: %v", err)
	}
	if smCfg.ServerInfo.Version != "v0.2.10-nullinfo" {
		t.Fatalf("stamped version = %q, want v0.2.10-nullinfo", smCfg.ServerInfo.Version)
	}
}

// spec: host-runtime / Embedded NATS JetStream and SemEngine SKG pipeline lifecycle
// spec: host-runtime / ADR-115 GraphQL front door and dual output/websocket streaming planes
func TestRuntimeStopDoesNotDeadlockInFlightPreReadyUIRequest(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	uiDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(uiDir, "index.html"), []byte("<html>cop</html>"), 0o600); err != nil {
		t.Fatalf("write index.html: %v", err)
	}

	rt, err := StartEphemeral(ctx, Options{
		DataDir: t.TempDir(),
		UIDir:   uiDir,
		Version: "v0.2.0-stop-race",
	}, 4)
	if err != nil {
		t.Fatalf("StartEphemeral: %v", err)
	}
	t.Cleanup(func() {
		rt.onReadinessCheck.Store(nil)
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cleanupCancel()
		_ = rt.Stop(cleanupCtx)
	})

	entered := make(chan struct{})
	releaseHook := make(chan struct{})
	defer close(releaseHook)
	var once sync.Once
	hook := func() {
		once.Do(func() {
			close(entered)
			deadline := time.Now().Add(500 * time.Millisecond)
			for !rt.stopping.Load() && time.Now().Before(deadline) {
				select {
				case <-releaseHook:
					return
				default:
					time.Sleep(2 * time.Millisecond)
				}
			}
			// Give Stop() time to acquire stopMu and enter ServiceManager.StopAll HTTP drain.
			time.Sleep(30 * time.Millisecond)
		})
	}
	rt.onReadinessCheck.Store(&hook)

	type httpResult struct {
		code int
		body string
		err  error
	}
	reqDone := make(chan httpResult, 1)
	go func() {
		client := &http.Client{Timeout: 5 * time.Second}
		resp, reqErr := client.Get(rt.Endpoints().HTTPBaseURL + "/") //nolint:noctx
		if reqErr != nil {
			reqDone <- httpResult{err: reqErr}
			return
		}
		raw, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		reqDone <- httpResult{code: resp.StatusCode, body: string(raw)}
	}()

	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for pre-ready GET / to enter checkLiveReadiness")
	}

	const stopBudget = 1500 * time.Millisecond
	stopStart := time.Now()
	stopCtx, stopCancel := context.WithTimeout(context.Background(), stopBudget)
	defer stopCancel()
	stopErr := rt.Stop(stopCtx)
	elapsed := time.Since(stopStart)
	if stopErr != nil || elapsed >= 1300*time.Millisecond {
		t.Fatalf("Stop blocked on in-flight pre-ready UI request (elapsed=%v, budget=%v, err=%v)", elapsed, stopBudget, stopErr)
	}

	select {
	case res := <-reqDone:
		if res.err != nil {
			t.Fatalf("in-flight pre-ready GET / failed: %v", res.err)
		}
		if res.code != http.StatusServiceUnavailable || res.body != "NOT READY" {
			t.Fatalf("in-flight pre-ready GET / = %d %q, want 503 NOT READY", res.code, res.body)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("in-flight pre-ready GET / did not complete after Stop")
	}
}
