package host

import (
	"bytes"
	"context"
	"fmt"
	"io/fs"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path"
	"strings"

	"github.com/c360studio/semengine/service"
)

const (
	telemetryWSFrontDoorPath = "/ws/telemetry"
	graphWSFrontDoorPath     = "/ws/graph"
)

// WebSocketProxyRoutes holds the resolved internal output/websocket URLs to
// expose at same-origin /ws/telemetry and /ws/graph on the HTTP front door,
// plus the optional read-only SKG search requester for /graph-gateway/search.
type WebSocketProxyRoutes struct {
	TelemetryWS string
	GraphWS     string
	GraphSearch GraphRequester
	ShutdownCtx context.Context
}

// NewStaticUIMiddleware serves compiled SvelteKit static assets from uiDir on
// unmatched GET/HEAD paths once isReady reports true, while passing all reserved
// SemEngine diagnostic, component, and ADR-115 GraphQL routes straight through.
func NewStaticUIMiddleware(uiDir string, isReady func() bool) service.HTTPMiddleware {
	mw, _ := NewUIMiddleware(uiDir, isReady, WebSocketProxyRoutes{})
	return mw
}

// NewUIMiddleware extends NewStaticUIMiddleware with same-origin WebSocket
// reverse-proxying for /ws/telemetry and /ws/graph and the read-only SKG
// POST /graph-gateway/search bridge gated on runtime readiness.
func NewUIMiddleware(uiDir string, isReady func() bool, wsRoutes WebSocketProxyRoutes) (service.HTTPMiddleware, error) {
	telemetryProxy, err := newSingleEndpointWSProxy("telemetry_ws", wsRoutes.TelemetryWS)
	if err != nil {
		return nil, err
	}
	graphProxy, err := newSingleEndpointWSProxy("graph_ws", wsRoutes.GraphWS)
	if err != nil {
		return nil, err
	}

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			normalizedPath := strings.ToLower(path.Clean("/" + r.URL.Path))
			if normalizedPath == GraphSearchFrontDoorPath {
				if r.Method != http.MethodPost {
					w.Header().Set("Allow", http.MethodPost)
					http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
					return
				}
				if isReady != nil && !isReady() {
					w.WriteHeader(http.StatusServiceUnavailable)
					_, _ = w.Write([]byte("NOT READY"))
					return
				}
				if wsRoutes.GraphSearch == nil {
					next.ServeHTTP(w, r)
					return
				}
				serveGraphSearch(w, r, wsRoutes.GraphSearch, wsRoutes.ShutdownCtx)
				return
			}

			wsProxy := matchWebSocketProxy(normalizedPath, telemetryProxy, graphProxy)
			if wsProxy != nil || isReservedWebSocketPath(normalizedPath) {
				if r.Method != http.MethodGet {
					http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
					return
				}
				if !isSameOriginWebSocketRequest(r) {
					http.Error(w, "forbidden websocket origin", http.StatusForbidden)
					return
				}
				if isReady != nil && !isReady() {
					w.WriteHeader(http.StatusServiceUnavailable)
					_, _ = w.Write([]byte("NOT READY"))
					return
				}
				if wsProxy == nil {
					next.ServeHTTP(w, r)
					return
				}
				if wsRoutes.ShutdownCtx != nil {
					reqCtx, cancel := context.WithCancel(r.Context())
					stopCancel := context.AfterFunc(wsRoutes.ShutdownCtx, cancel)
					defer func() {
						stopCancel()
						cancel()
					}()
					r = r.WithContext(reqCtx)
				}
				wsProxy.ServeHTTP(w, r)
				return
			}

			if r.Method != http.MethodGet && r.Method != http.MethodHead {
				next.ServeHTTP(w, r)
				return
			}
			if isReservedBackendPath(r.URL.Path) {
				next.ServeHTTP(w, r)
				return
			}
			if isReady != nil && !isReady() {
				w.WriteHeader(http.StatusServiceUnavailable)
				_, _ = w.Write([]byte("NOT READY"))
				return
			}
			if uiDir == "" {
				next.ServeHTTP(w, r)
				return
			}
			dirInfo, err := os.Stat(uiDir)
			if err != nil || !dirInfo.IsDir() {
				next.ServeHTTP(w, r)
				return
			}

			fsys := os.DirFS(uiDir)
			fileServer := http.FileServerFS(fsys)
			cleaned := strings.TrimPrefix(path.Clean("/"+r.URL.Path), "/")
			if cleaned == "" || cleaned == "." {
				if _, err := fs.Stat(fsys, "index.html"); err == nil {
					serveIndexHTML(w, r, fsys)
					return
				}
				next.ServeHTTP(w, r)
				return
			}

			if info, err := fs.Stat(fsys, cleaned); err == nil {
				if !info.IsDir() {
					fileServer.ServeHTTP(w, r)
					return
				}
				if _, idxErr := fs.Stat(fsys, path.Join(cleaned, "index.html")); idxErr == nil {
					fileServer.ServeHTTP(w, r)
					return
				}
			}

			// SvelteKit static SPA fallback to index.html for client-side routes.
			if _, err := fs.Stat(fsys, "index.html"); err == nil {
				serveIndexHTML(w, r, fsys)
				return
			}
			next.ServeHTTP(w, r)
		})
	}, nil
}

func isSameOriginWebSocketRequest(r *http.Request) bool {
	origins := r.Header.Values("Origin")
	if len(origins) == 0 {
		return true
	}
	if len(origins) > 1 {
		return false
	}
	rawOrigin := strings.TrimSpace(origins[0])
	if rawOrigin == "" {
		return false
	}
	originURL, err := url.Parse(rawOrigin)
	if err != nil || originURL.Host == "" || originURL.User != nil ||
		originURL.Path != "" || originURL.RawQuery != "" || originURL.Fragment != "" {
		return false
	}
	reqScheme := "http"
	if r.TLS != nil {
		reqScheme = "https"
	}
	if strings.ToLower(originURL.Scheme) != reqScheme {
		return false
	}
	reqHost := strings.TrimSpace(r.Host)
	if reqHost == "" {
		return false
	}
	reqURL, err := url.Parse(reqScheme + "://" + reqHost)
	if err != nil || reqURL.Host == "" || reqURL.User != nil ||
		reqURL.Path != "" || reqURL.RawQuery != "" || reqURL.Fragment != "" {
		return false
	}
	originHostname := originURL.Hostname()
	reqHostname := reqURL.Hostname()
	if originHostname == "" || reqHostname == "" || !strings.EqualFold(originHostname, reqHostname) {
		return false
	}
	defaultPort := "80"
	if reqScheme == "https" {
		defaultPort = "443"
	}
	originPort := originURL.Port()
	if originPort == "" {
		originPort = defaultPort
	}
	reqPort := reqURL.Port()
	if reqPort == "" {
		reqPort = defaultPort
	}
	return originPort == reqPort
}

func newSingleEndpointWSProxy(label, rawTarget string) (http.Handler, error) {
	trimmed := strings.TrimSpace(rawTarget)
	if trimmed == "" {
		return nil, nil
	}
	parsed, err := url.Parse(trimmed)
	if err != nil || parsed.Host == "" {
		return nil, fmt.Errorf("invalid %s URL %q", label, rawTarget)
	}
	var scheme string
	switch strings.ToLower(parsed.Scheme) {
	case "ws", "http":
		scheme = "http"
	case "wss", "https":
		scheme = "https"
	default:
		return nil, fmt.Errorf("unsupported %s scheme %q", label, parsed.Scheme)
	}
	targetPath := parsed.Path
	if targetPath == "" {
		targetPath = "/ws"
	}
	upstreamBase := &url.URL{
		Scheme: scheme,
		Host:   parsed.Host,
	}
	proxy := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(upstreamBase)
			pr.Out.URL.Path = targetPath
			pr.Out.URL.RawPath = ""
			pr.Out.Host = upstreamBase.Host
			pr.SetXForwarded()
		},
	}
	return proxy, nil
}

func matchWebSocketProxy(normalizedPath string, telemetryProxy, graphProxy http.Handler) http.Handler {
	switch normalizedPath {
	case telemetryWSFrontDoorPath:
		return telemetryProxy
	case graphWSFrontDoorPath:
		return graphProxy
	default:
		return nil
	}
}

func isReservedWebSocketPath(normalizedPath string) bool {
	return normalizedPath == "/ws" || strings.HasPrefix(normalizedPath, "/ws/")
}

func serveIndexHTML(w http.ResponseWriter, r *http.Request, fsys fs.FS) {
	data, err := fs.ReadFile(fsys, "index.html")
	if err != nil {
		http.Error(w, "failed to read UI index", http.StatusInternalServerError)
		return
	}
	info, err := fs.Stat(fsys, "index.html")
	if err != nil {
		http.Error(w, "failed to stat UI index", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	http.ServeContent(w, r, "index.html", info.ModTime(), bytes.NewReader(data))
}

func isReservedBackendPath(urlPath string) bool {
	cleaned := strings.ToLower(path.Clean("/" + urlPath))
	switch cleaned {
	case "/health", "/healthz", "/readyz", "/services", "/openapi.json", "/docs",
		"/components", "/graph", "/graph-gateway", "/message-logger", "/storage-observability":
		return true
	}
	for _, prefix := range []string{
		"/services/",
		"/docs/",
		"/components/",
		"/graph/",
		"/graph-gateway/",
		"/message-logger/",
		"/storage-observability/",
	} {
		if strings.HasPrefix(cleaned, prefix) {
			return true
		}
	}
	return false
}
