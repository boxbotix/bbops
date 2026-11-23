package host

import (
	"bytes"
	"io/fs"
	"net/http"
	"os"
	"path"
	"strings"

	"github.com/c360studio/semengine/service"
)

// NewStaticUIMiddleware serves compiled SvelteKit static assets from uiDir on
// unmatched GET/HEAD paths once isReady reports true, while passing all reserved
// SemEngine diagnostic, component, and ADR-115 GraphQL routes straight through.
func NewStaticUIMiddleware(uiDir string, isReady func() bool) service.HTTPMiddleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
	}
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
