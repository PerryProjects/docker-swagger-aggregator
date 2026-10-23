package server

import (
	"context"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"

	"go.uber.org/zap"
)

type targetKey struct{}

// bufferPool implements [httputil.BufferPool].
type bufferPool struct{ pool sync.Pool }

func newBufferPool() *bufferPool {
	return &bufferPool{pool: sync.Pool{New: func() any { b := make([]byte, proxyBufferSize); return &b }}}
}

func (p *bufferPool) Get() []byte {
	b, ok := p.pool.Get().(*[]byte)
	if !ok {
		return make([]byte, proxyBufferSize)
	}
	return *b
}

func (p *bufferPool) Put(b []byte) { p.pool.Put(&b) }

// newReverseProxy builds the single proxy shared by all requests. The target
// travels in the request context, so no per-request proxy is allocated.
func (s *Server) newReverseProxy() *httputil.ReverseProxy {
	return &httputil.ReverseProxy{
		Transport:     s.cfg.Transport,
		FlushInterval: -1, // stream responses (SSE etc.) immediately
		BufferPool:    newBufferPool(),
		Rewrite: func(pr *httputil.ProxyRequest) {
			target, ok := pr.In.Context().Value(targetKey{}).(*url.URL)
			if !ok { // cannot happen: forward always sets it; fail closed with a transport error
				target = &url.URL{}
			}
			pr.Out.URL = target
			pr.Out.Host = target.Host
			// The UI's own credentials and origin are none of the service's business.
			pr.Out.Header.Del("Cookie")
			pr.Out.Header.Del("Origin")
			pr.Out.Header.Del("Referer")
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			target, _ := r.Context().Value(targetKey{}).(*url.URL)
			host := ""
			if target != nil {
				host = target.Host
			}
			s.cfg.Log.Warn("proxy error", zap.String("target", host), zap.Error(err))
			http.Error(w, "upstream unreachable", http.StatusBadGateway)
		},
	}
}

// byID is the Try-it proxy for specs without a server-url: /api/proxy/<id>/....
// The target is looked up by workload ID in the registry, never taken from the
// request, so it cannot be used to reach arbitrary hosts.
func (s *Server) byID() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.forwardToID(w, r, r.PathValue("id"), r.PathValue("path"), r.URL.RawQuery)
	})
}

// byPublicURL is Scalar's `proxyUrl` endpoint: the browser calls
// /api/scalar-proxy?scalar_url=<public URL> for every request whose URL is
// absolute (e.g. a service's server-url). Only URLs under a registered
// service's server-url are accepted; they are mapped to that workload.
func (s *Server) byPublicURL() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		target, err := url.Parse(r.URL.Query().Get("scalar_url"))
		if err != nil || (target.Scheme != "http" && target.Scheme != "https") || target.Host == "" {
			http.Error(w, "invalid scalar_url", http.StatusBadRequest)
			return
		}
		// Scalar also sends our own relative /api/proxy/<id> servers through here once
		// it has resolved them to absolute URLs. Only the path matters: the host is
		// never contacted, so this grants nothing beyond calling /api/proxy/<id> directly.
		if rest, found := strings.CutPrefix(target.Path, "/api/proxy/"); found {
			id, path, _ := strings.Cut(rest, "/")
			s.forwardToID(w, r, id, path, target.RawQuery)
			return
		}
		to, ok := s.cfg.Store.Route(target)
		if !ok {
			http.Error(w, "no registered service for this URL", http.StatusNotFound)
			return
		}
		s.forward(w, r, to)
	})
}

func (s *Server) forwardToID(w http.ResponseWriter, r *http.Request, id, path, rawQuery string) {
	base, ok := s.cfg.Store.Base(id)
	target, err := url.Parse(base)
	if !ok || err != nil {
		http.Error(w, "unknown or stopped service", http.StatusNotFound)
		return
	}
	target.Path = strings.TrimRight(target.Path, "/") + "/" + path // keep a base path prefix
	target.RawQuery = rawQuery
	s.forward(w, r, target)
}

func (s *Server) forward(w http.ResponseWriter, r *http.Request, target *url.URL) {
	r.Body = http.MaxBytesReader(w, r.Body, s.cfg.MaxProxyBody)
	s.proxy.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), targetKey{}, target)))
}

// ProxyPath is the same-origin path under which the API proxies a workload.
func ProxyPath(id string) string { return "/api/proxy/" + id }
