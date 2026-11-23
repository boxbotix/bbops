package host

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

// spec: host-runtime / Host CLI version and flag entrypoint
// spec: host-runtime / Embedded NATS JetStream and SemEngine SKG pipeline lifecycle
func TestValidateOfflineAndPortOverrides(t *testing.T) {
	t.Run("embedded default flow validates all seven components and endpoints", func(t *testing.T) {
		cfg, err := LoadConfig("", PortOverrides{})
		if err != nil {
			t.Fatalf("LoadConfig default: %v", err)
		}
		res, err := ValidateOffline(cfg)
		if err != nil {
			t.Fatalf("ValidateOffline default: %v", err)
		}
		if got, want := len(res.Graph.Nodes), 7; got != want {
			t.Fatalf("admitted component nodes = %d, want %d", got, want)
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
	})

	t.Run("ENTITY stream missing finite retention or discard old fails offline check", func(t *testing.T) {
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

	t.Run("port overrides enforce 1024..65535 range and reject port collisions", func(t *testing.T) {
		if _, err := LoadConfig("", PortOverrides{HTTPPort: 1023}); err == nil {
			t.Fatal("expected error for HTTPPort < 1024")
		}
		if _, err := LoadConfig("", PortOverrides{TelemetryWSPort: 65536}); err == nil {
			t.Fatal("expected error for TelemetryWSPort > 65535")
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
