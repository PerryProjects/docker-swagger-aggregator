// Package server exposes the registry over HTTP: the JSON/SSE API, the Try-it
// proxies and a static web server for the built UI.
package server

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/perryprojects/docker-swagger-aggregator/internal/registry"
)

const (
	defaultHeartbeat    = 15 * time.Second
	defaultMaxProxyBody = 10 << 20
	dialTimeout         = 5 * time.Second
	upstreamHeaderWait  = 30 * time.Second
	proxyBufferSize     = 32 << 10

	sseServices  = "event: services\ndata: "
	sseEnd       = "\n\n"
	sseHeartbeat = ": heartbeat\n\n"
)

// Store is what the server needs from the registry.
type Store interface {
	List() []registry.Service
	Spec(name string) (spec []byte, hash string, found bool)
	Base(id string) (string, bool)
	Route(target *url.URL) (*url.URL, bool)
	Subscribe() (changed <-chan struct{}, unsubscribe func())
	Version() uint64
}

// Config holds the Server's dependencies; only Store and Log are required.
type Config struct {
	Store Store
	Log   *zap.Logger
	// Transport is used to reach the workloads for the proxies. Defaults to
	// NewUpstreamTransport().
	Transport http.RoundTripper
	// BasePath is the public prefix the UI and API are served under ("" or "/docs"), see ProxyPath.
	BasePath string
	// Heartbeat is the SSE keep-alive interval (default 15s).
	Heartbeat time.Duration
	// MaxProxyBody limits proxied request bodies in bytes (default 10 MiB).
	MaxProxyBody int64
}

// Server serves the API routes.
type Server struct {
	cfg   Config
	proxy *httputil.ReverseProxy
	snap  snapshotter
}

// New creates a Server.
func New(cfg Config) *Server {
	if cfg.Transport == nil {
		cfg.Transport = NewUpstreamTransport()
	}
	if cfg.Heartbeat <= 0 {
		cfg.Heartbeat = defaultHeartbeat
	}
	if cfg.MaxProxyBody <= 0 {
		cfg.MaxProxyBody = defaultMaxProxyBody
	}
	s := &Server{cfg: cfg, snap: snapshotter{store: cfg.Store}}
	s.proxy = s.newReverseProxy()
	return s
}

// NewUpstreamTransport returns a transport with dial and response-header
// timeouts, so a hung workload cannot pin a connection forever.
func NewUpstreamTransport() *http.Transport {
	t, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		return &http.Transport{}
	}
	t = t.Clone()
	t.DialContext = (&net.Dialer{Timeout: dialTimeout}).DialContext
	t.ResponseHeaderTimeout = upstreamHeaderWait
	return t
}

// Handler returns the API routes.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { fmt.Fprintln(w, "ok") })
	mux.HandleFunc("GET /api/services", s.list)
	mux.HandleFunc("GET /api/services/{name}/spec", s.spec)
	mux.HandleFunc("GET /api/events", s.events)
	mux.Handle("/api/proxy/{id}/{path...}", s.byID())
	mux.Handle("/api/scalar-proxy", s.byPublicURL())
	return mux
}

func (s *Server) list(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-cache")
	if err := json.NewEncoder(w).Encode(s.cfg.Store.List()); err != nil {
		s.cfg.Log.Debug("write services failed", zap.Error(err))
	}
}

func (s *Server) spec(w http.ResponseWriter, r *http.Request) {
	spec, hash, found := s.cfg.Store.Spec(r.PathValue("name"))
	switch {
	case !found:
		http.Error(w, "unknown service", http.StatusNotFound)
	case spec == nil:
		http.Error(w, "spec not fetched yet", http.StatusNotFound)
	default:
		etag := `"` + hash + `"`
		w.Header().Set("ETag", etag)
		w.Header().Set("Cache-Control", "no-cache") // always revalidate; 304 is cheap
		if r.Header.Get("If-None-Match") == etag {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		//nolint:gosec // G705: spec is canonical JSON served as application/json with nosniff, never rendered as HTML
		if _, err := w.Write(spec); err != nil {
			s.cfg.Log.Debug("write spec failed", zap.Error(err))
		}
	}
}

// events streams the full service list as SSE: once on connect, then on every
// change, with a comment heartbeat so idle proxies keep the connection open.
func (s *Server) events(w http.ResponseWriter, r *http.Request) {
	rc := http.NewResponseController(w)
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Accel-Buffering", "no") // nginx; Traefik does not buffer SSE
	// Subscribe before the first snapshot so no change falls in between.
	changed, unsubscribe := s.cfg.Store.Subscribe()
	defer unsubscribe()

	snapshot := func() bool {
		b, err := s.snap.get()
		if err != nil {
			return false
		}
		_, e1 := io.WriteString(w, sseServices)
		_, e2 := w.Write(b)
		_, e3 := io.WriteString(w, sseEnd)
		return e1 == nil && e2 == nil && e3 == nil && rc.Flush() == nil
	}
	heartbeat := func() bool {
		_, err := io.WriteString(w, sseHeartbeat)
		return err == nil && rc.Flush() == nil
	}

	if !snapshot() {
		return
	}
	tick := time.NewTicker(s.cfg.Heartbeat)
	defer tick.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-changed:
			if !snapshot() {
				return
			}
		case <-tick.C:
			if !heartbeat() {
				return
			}
		}
	}
}

// snapshotter caches the marshaled service list per registry version, so any
// number of SSE clients cost one marshal per change instead of one each.
type snapshotter struct {
	store Store

	mu      sync.Mutex
	version uint64
	json    []byte
	valid   bool
}

func (s *snapshotter) get() ([]byte, error) {
	// Read the version before the list: a change in between only causes one
	// extra rebuild, never a stale snapshot.
	v := s.store.Version()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.valid && s.version == v {
		return s.json, nil
	}
	b, err := json.Marshal(s.store.List())
	if err != nil {
		return nil, err
	}
	s.json, s.version, s.valid = b, v, true
	return b, nil
}
