// Package app is the composition root: it builds the registry, the discovery
// sources and the HTTP server from a Config and injectable Deps.
package app

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/moby/moby/client"
	"go.uber.org/zap"
	"k8s.io/client-go/dynamic"
	kube "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/perryprojects/docker-swagger-aggregator/internal/discovery"
	"github.com/perryprojects/docker-swagger-aggregator/internal/discovery/docker"
	"github.com/perryprojects/docker-swagger-aggregator/internal/discovery/kubernetes"
	"github.com/perryprojects/docker-swagger-aggregator/internal/fetcher"
	"github.com/perryprojects/docker-swagger-aggregator/internal/registry"
	"github.com/perryprojects/docker-swagger-aggregator/internal/server"
	"github.com/perryprojects/docker-swagger-aggregator/internal/transport"
)

const accessAuto = "auto"

const (
	readHeaderTimeout    = 10 * time.Second
	defaultShutdownGrace = 5 * time.Second
	defaultProbeInterval = 15 * time.Second
	defaultFetchTimeout  = 5 * time.Second
	defaultMaxSpecBytes  = 10 << 20
)

// Config is the validated runtime configuration.
type Config struct {
	Listen               string        // HTTP listen address
	DockerHost           string        // empty: DOCKER_HOST, else the local socket
	Network              string        // preferred Docker network; empty: first with an IP
	LabelPrefix          string        // attribute namespace to watch
	ProbeInterval        time.Duration // spec re-fetch (liveness probe) interval; retry backoff cap
	FetchTimeout         time.Duration // per-request timeout when fetching a spec
	MaxSpecBytes         int64
	ShutdownGrace        time.Duration // how long in-flight requests get to finish on shutdown
	DeriveServerURL      bool          // derive server-url from Traefik labels / Ingress / IngressRoute
	PreferPublishedPorts bool
	// InsecureUpstreamTLS skips certificate verification for workloads labeled scheme=https
	// (workloads are reached by IP, which their certificates rarely cover).
	InsecureUpstreamTLS bool
	// BasePath is the public path prefix the UI is served under ("" or "/docs"); it only affects the
	// same-origin proxy paths written into the specs.
	BasePath string

	DockerEnabled bool // watch Docker containers
	Kubernetes    KubernetesConfig
}

// KubernetesConfig configures the Kubernetes source.
type KubernetesConfig struct {
	Enabled    bool
	Kubeconfig string   // path; empty: in-cluster config if available, else KUBECONFIG / ~/.kube/config
	Context    string   // kubeconfig context; empty: the current one
	Namespaces []string // empty: all namespaces
	// Access is "service" (in-cluster DNS), "apiserver" (through the API server's
	// service proxy, works from outside the cluster) or "auto" (service when running
	// in a cluster, apiserver otherwise).
	Access        string
	ClusterDomain string // DNS suffix for access=service (default cluster.local)
}

// DefaultConfig returns the defaults, also used for the CLI flag defaults.
func DefaultConfig() Config {
	return Config{
		Listen:        ":8080",
		LabelPrefix:   discovery.DefaultPrefix,
		ProbeInterval: defaultProbeInterval,
		FetchTimeout:  defaultFetchTimeout,
		MaxSpecBytes:  defaultMaxSpecBytes,
		ShutdownGrace: defaultShutdownGrace,
		DockerEnabled: true,
		Kubernetes:    KubernetesConfig{Access: accessAuto, ClusterDomain: "cluster.local"},
	}
}

// Deps are the replaceable collaborators. The zero value of every field means
// "build the real one from Config".
type Deps struct {
	Log *zap.Logger
	// Docker replaces the Docker client (tests inject a fake).
	Docker docker.API
	// Transport replaces the transport the Try-it proxies use for ordinary workloads.
	Transport http.RoundTripper
	// Kubernetes replaces the Kubernetes clients (tests inject fakes); Access is then "service".
	Kubernetes *kubernetes.Clients
}

// App wires and runs the whole service.
type App struct {
	cfg     Config
	log     *zap.Logger
	sources []discovery.Source
	mgr     *discovery.Manager
	handler http.Handler
	closers []io.Closer
}

// New builds the object graph. Call Close (or let Run do it) to release resources.
func New(cfg Config, deps Deps) (*App, error) {
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("invalid config: %w", err)
	}
	log := deps.Log
	if log == nil {
		log = zap.NewNop()
	}
	a := &App{cfg: cfg, log: log}

	// Hosts that need their own credentials (the Kubernetes API server's service
	// proxy) get an authenticated transport; everything else uses the defaults.
	byHost := map[string]http.RoundTripper{}

	if cfg.DockerEnabled {
		api := deps.Docker
		if api == nil {
			cli, err := newDockerClient(cfg.DockerHost)
			if err != nil {
				return nil, fmt.Errorf("docker client: %w", err)
			}
			a.closers = append(a.closers, cli)
			api = cli
		}
		a.sources = append(a.sources, docker.New(api, docker.Config{
			Network:              cfg.Network,
			LabelPrefix:          cfg.LabelPrefix,
			TraefikServerURL:     cfg.DeriveServerURL,
			PreferPublishedPorts: cfg.PreferPublishedPorts,
		}, log))
	}
	if cfg.Kubernetes.Enabled {
		src, err := newKubernetesSource(cfg, deps, byHost, log)
		if err != nil {
			_ = a.Close()
			return nil, err
		}
		a.sources = append(a.sources, src)
	}

	reg := registry.New(registry.Config{})
	a.mgr = discovery.NewManager(discovery.ManagerConfig{
		Store: reg,
		Fetcher: fetcher.New(fetcher.Config{
			Client: &http.Client{
				Timeout:   cfg.FetchTimeout,
				Transport: &transport.Mux{Default: upstreamTransport(cfg), ByHost: byHost},
			},
			MaxBytes: cfg.MaxSpecBytes,
		}),
		Prepare: func(raw []byte, serverURL, id string) ([]byte, error) {
			return fetcher.Prepare(raw, serverURL, server.ProxyPath(cfg.BasePath, id))
		},
		Log:      log,
		Interval: cfg.ProbeInterval,
	})
	upstream := deps.Transport
	if upstream == nil {
		upstream = upstreamTransport(cfg)
	}
	a.handler = server.New(server.Config{
		Store: reg, Log: log, BasePath: cfg.BasePath, Transport: &transport.Mux{Default: upstream, ByHost: byHost},
	}).Handler()
	return a, nil
}

// upstreamTransport reaches the workloads (spec fetch and Try-it proxies).
func upstreamTransport(cfg Config) *http.Transport {
	t := server.NewUpstreamTransport()
	if cfg.InsecureUpstreamTLS {
		t.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} //nolint:gosec // explicit opt-in, workload traffic only
	}
	return t
}

// newKubernetesSource builds the clients (unless injected), decides how services
// are reached and registers an authenticated transport for the API server host
// when going through its service proxy.
func newKubernetesSource(
	cfg Config,
	deps Deps,
	byHost map[string]http.RoundTripper,
	log *zap.Logger,
) (*kubernetes.Source, error) {
	kc := cfg.Kubernetes
	srvCfg := kubernetes.Config{
		Namespaces:      kc.Namespaces,
		LabelPrefix:     cfg.LabelPrefix,
		DeriveServerURL: cfg.DeriveServerURL,
		ClusterDomain:   kc.ClusterDomain,
		Access:          kubernetes.AccessService,
	}
	if deps.Kubernetes != nil {
		return kubernetes.New(*deps.Kubernetes, srvCfg, log), nil
	}

	restCfg, inCluster, err := restConfig(kc)
	if err != nil {
		return nil, fmt.Errorf("kubernetes config: %w", err)
	}
	access := kubernetes.Access(kc.Access)
	if kc.Access == accessAuto {
		access = kubernetes.AccessAPIServer
		if inCluster {
			access = kubernetes.AccessService
		}
	}
	if access == kubernetes.AccessService && !inCluster {
		return nil, FieldError{
			Field: "kube-access",
			Message: "service (in-cluster DNS) only works when running inside the cluster, " +
				"but no ServiceAccount was found; use apiserver or auto, or run in a pod",
		}
	}
	srvCfg.Access = access
	if access == kubernetes.AccessAPIServer {
		rt, rtErr := rest.TransportFor(restCfg)
		if rtErr != nil {
			return nil, fmt.Errorf("kubernetes transport: %w", rtErr)
		}
		apiBase, host, hostErr := apiServerURL(restCfg.Host)
		if hostErr != nil {
			return nil, hostErr
		}
		byHost[host] = rt
		srvCfg.APIServer = apiBase
	}
	core, err := kube.NewForConfig(restCfg)
	if err != nil {
		return nil, fmt.Errorf("kubernetes client: %w", err)
	}
	dyn, err := dynamic.NewForConfig(restCfg)
	if err != nil {
		return nil, fmt.Errorf("kubernetes dynamic client: %w", err)
	}
	return kubernetes.New(kubernetes.Clients{Core: core, Dynamic: dyn}, srvCfg, log), nil
}

// restConfig prefers an explicit kubeconfig/context, then the in-cluster
// ServiceAccount, then the default kubeconfig (KUBECONFIG, ~/.kube/config).
func restConfig(kc KubernetesConfig) (*rest.Config, bool, error) {
	if kc.Kubeconfig == "" && kc.Context == "" {
		if c, err := rest.InClusterConfig(); err == nil {
			return c, true, nil
		} else if !errors.Is(err, rest.ErrNotInCluster) {
			return nil, false, err
		}
	}
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	if kc.Kubeconfig != "" {
		rules.ExplicitPath = kc.Kubeconfig
	}
	c, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		rules, &clientcmd.ConfigOverrides{CurrentContext: kc.Context}).ClientConfig()
	return c, false, err
}

// apiServerURL normalises rest.Config.Host to a base URL and its host:port.
func apiServerURL(host string) (string, string, error) {
	if !strings.Contains(host, "://") {
		host = "https://" + host
	}
	u, err := url.Parse(host)
	if err != nil || u.Host == "" {
		return "", "", fmt.Errorf("invalid kubernetes API server address %q", host)
	}
	return u.Scheme + "://" + u.Host + strings.TrimRight(u.Path, "/"), u.Host, nil
}

// newDockerClient honours DOCKER_HOST; an explicit host wins.
func newDockerClient(host string) (*client.Client, error) {
	opts := []client.Opt{client.FromEnv}
	if host != "" {
		opts = append(opts, client.WithHost(host))
	}
	return client.New(opts...)
}

// Handler returns the API handler (for tests and embedding).
func (a *App) Handler() http.Handler { return a.handler }

// Close releases resources held by New.
func (a *App) Close() error {
	var errs []error
	for _, c := range a.closers {
		errs = append(errs, c.Close())
	}
	a.closers = nil
	return errors.Join(errs...)
}

// Run listens on Config.Listen and serves until ctx is done.
func (a *App) Run(ctx context.Context) error {
	ln, err := (&net.ListenConfig{}).Listen(ctx, "tcp", a.cfg.Listen)
	if err != nil {
		_ = a.Close()
		return err
	}
	a.log.Info("listening", zap.String("addr", ln.Addr().String()))
	return a.Serve(ctx, ln)
}

// Serve runs the discovery sources and the HTTP server on ln until ctx is done
// or the server fails, then shuts both down gracefully and calls Close.
func (a *App) Serve(ctx context.Context, ln net.Listener) error {
	defer a.Close()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	var wg sync.WaitGroup
	for _, s := range a.sources {
		wg.Go(func() { s.Run(ctx, a.mgr) })
	}
	defer wg.Wait()

	// No WriteTimeout: it would kill the long-lived SSE responses.
	srv := &http.Server{Handler: a.handler, ReadHeaderTimeout: readHeaderTimeout}
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()

	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
		shutdown, stop := context.WithTimeout(context.WithoutCancel(ctx), a.cfg.ShutdownGrace)
		defer stop()
		// SSE handlers return via their request context. Connections that never sent a
		// request count as active until the grace period ends; then force them closed
		// rather than failing the shutdown.
		if err := srv.Shutdown(shutdown); err != nil {
			a.log.Warn("graceful shutdown timed out, closing connections", zap.Error(err))
			return srv.Close()
		}
		return nil
	}
}
