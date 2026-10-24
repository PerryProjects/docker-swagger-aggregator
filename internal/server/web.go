package server

import (
	"fmt"
	"io/fs"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
)

// WebConfig holds the dependencies of the static web server.
type WebConfig struct {
	// UI is the built frontend (index.html at its root).
	UI fs.FS
	// API is the base URL /api/ is forwarded to.
	API *url.URL
	// Transport reaches the API (default: http.DefaultTransport).
	Transport http.RoundTripper
	// BasePath is the public prefix the UI is served under ("" or "/docs"). It is stripped
	// when present, so it works whether or not the reverse proxy in front strips it.
	BasePath string
	// Protect wraps everything the server answers (UI, /api, the proxies, SSE), e.g. with a login.
	// It sees paths without the BasePath. nil: no protection.
	Protect func(http.Handler) http.Handler
	// MCP serves the Model Context Protocol at /mcp. It sits outside Protect on purpose: AI clients cannot
	// do a browser login, so the handler has to authenticate itself (a bearer token). nil: no /mcp.
	MCP http.Handler
}

// NewWebHandler serves the built UI and forwards /api/ to the API, so the
// browser sees a single origin (needed for the SSE stream and the proxies).
func NewWebHandler(cfg WebConfig) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { fmt.Fprintln(w, "ok") })
	mux.Handle("/api/", &httputil.ReverseProxy{
		Transport:     cfg.Transport,
		Rewrite:       func(pr *httputil.ProxyRequest) { pr.SetURL(cfg.API) },
		FlushInterval: -1, // SSE must not be buffered
		BufferPool:    newBufferPool(),
	})
	mux.Handle("/", spa(cfg.UI))
	var h http.Handler = mux
	if cfg.Protect != nil {
		h = cfg.Protect(h)
	}
	if cfg.MCP != nil {
		outer := http.NewServeMux()
		outer.Handle("/mcp", cfg.MCP)
		outer.Handle("/", h)
		h = outer
	}
	return securityHeaders(stripBasePath(cfg.BasePath, h))
}

// stripBasePath removes the public prefix from the request path (a no-op for "").
func stripBasePath(base string, next http.Handler) http.Handler {
	if base == "" {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if rest, ok := strings.CutPrefix(r.URL.Path, base); ok && (rest == "" || rest[0] == '/') {
			r2 := r.Clone(r.Context())
			r2.URL.Path = "/" + strings.TrimPrefix(rest, "/")
			r2.URL.RawPath = ""
			r = r2
		}
		next.ServeHTTP(w, r)
	})
}

// securityHeaders sets conservative defaults for everything the web server
// answers. No CSP yet: Scalar's inline styles/workers need testing first.
func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "same-origin")
		h.Set("X-Frame-Options", "DENY")
		next.ServeHTTP(w, r)
	})
}

// spa serves static files and falls back to index.html for unknown paths.
// Vite emits content-hashed files under /assets/, which never change.
func spa(ui fs.FS) http.Handler {
	files := http.FileServerFS(ui)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if p := r.URL.Path[1:]; p != "" {
			if _, err := fs.Stat(ui, p); err != nil {
				r.URL.Path = "/"
			}
		}
		if strings.HasPrefix(r.URL.Path, "/assets/") {
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		} else {
			w.Header().Set("Cache-Control", "no-cache")
		}
		files.ServeHTTP(w, r)
	})
}
