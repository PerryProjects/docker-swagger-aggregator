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
	return securityHeaders(mux)
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
