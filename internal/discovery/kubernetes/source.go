// Package kubernetes is a discovery.Source for Kubernetes Services.
//
// A Service labeled `<prefix>.enable=true` is documented. Its readiness comes
// from EndpointSlices; its public URL can be derived from Ingress or Traefik
// IngressRoute objects that route to it. Everything is driven by informers and a
// level-triggered reconcile: on any change the desired state is recomputed from
// the caches and only the differences are reported.
package kubernetes

import (
	"context"
	"fmt"
	"maps"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/informers"
	kube "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"

	"github.com/perryprojects/docker-swagger-aggregator/internal/discovery"
)

// Access says how the aggregator reaches a Service.
type Access string

const (
	// AccessService uses the in-cluster DNS name; the aggregator must run in the cluster.
	AccessService Access = "service"
	// AccessAPIServer goes through the API server's service proxy
	// (/api/v1/namespaces/<ns>/services/<name>:<port>/proxy); works from outside the
	// cluster, needs `get` on services/proxy and an authenticated transport for the API server host.
	AccessAPIServer Access = "apiserver"
)

const (
	defaultClusterDomain = "cluster.local"
	defaultResync        = 10 * time.Minute
	defaultDebounce      = 100 * time.Millisecond
	defaultSyncTimeout   = 30 * time.Second
	crdRecheck           = time.Minute
)

// SourceName is the value of discovery.Container.Source for Kubernetes Services.
const SourceName = "kubernetes"

// Clients are the Kubernetes clients the source uses.
type Clients struct {
	Core kube.Interface
	// Dynamic is used for Traefik IngressRoute objects; nil disables that part.
	Dynamic dynamic.Interface
}

// Config configures a Source. Only LabelPrefix and Access are required.
type Config struct {
	// Namespaces to watch; empty means all (needs cluster-wide RBAC).
	Namespaces []string
	// LabelPrefix is the attribute namespace (default discovery.DefaultPrefix).
	LabelPrefix string
	// DeriveServerURL derives server-url from Ingress/IngressRoute objects when
	// the Service has no explicit server-url annotation.
	DeriveServerURL bool
	Access          Access
	// APIServer is the API server base URL (https://host:port), for AccessAPIServer.
	APIServer string
	// ClusterDomain is the DNS suffix for AccessService (default cluster.local).
	ClusterDomain string
	// Resync is the informers' full resync period (default 10m).
	Resync time.Duration
	// Debounce coalesces bursts of events before reconciling (default 100ms).
	Debounce time.Duration
	// CRDRecheck is how often to look for the Traefik IngressRoute CRD until it
	// exists (default 1m).
	CRDRecheck time.Duration
	// SyncTimeout bounds the wait for the initial cache sync (default 30s). Missing
	// RBAC for one resource must not block the others forever.
	SyncTimeout time.Duration
}

// Source watches Services, EndpointSlices, Ingresses, IngressRoutes and Gateway API HTTPRoutes.
type Source struct {
	cl  Clients
	cfg Config
	log *zap.Logger

	mu     sync.RWMutex
	caches caches
}

// New creates a Source.
func New(cl Clients, cfg Config, log *zap.Logger) *Source {
	if cfg.LabelPrefix == "" {
		cfg.LabelPrefix = discovery.DefaultPrefix
	}
	if cfg.Access == "" {
		cfg.Access = AccessService
	}
	if cfg.ClusterDomain == "" {
		cfg.ClusterDomain = defaultClusterDomain
	}
	if cfg.Resync <= 0 {
		cfg.Resync = defaultResync
	}
	if cfg.Debounce <= 0 {
		cfg.Debounce = defaultDebounce
	}
	if cfg.CRDRecheck <= 0 {
		cfg.CRDRecheck = crdRecheck
	}
	if cfg.SyncTimeout <= 0 {
		cfg.SyncTimeout = defaultSyncTimeout
	}
	return &Source{cl: cl, cfg: cfg, log: log}
}

// Run blocks until ctx is done.
func (s *Source) Run(ctx context.Context, h discovery.Handler) {
	trigger := make(chan struct{}, 1)
	poke := func() {
		select {
		case trigger <- struct{}{}:
		default: // already pending
		}
	}
	events := cache.ResourceEventHandlerFuncs{
		AddFunc:    func(any) { poke() },
		UpdateFunc: func(_, _ any) { poke() },
		DeleteFunc: func(any) { poke() },
	}

	synced := s.startCore(ctx, events)
	s.waitForSync(ctx, synced)
	if ctx.Err() != nil {
		return
	}
	if s.cl.Dynamic != nil {
		go s.watchCRD(ctx, ingressRouteGVR(), "Traefik IngressRoute", events,
			func(c *caches, l []cache.GenericLister) { c.routes = append(c.routes, l...) })
		if s.cfg.DeriveServerURL { // Gateway API: only needed to derive URLs
			go s.watchCRD(ctx, gatewayGVR(), "Gateway API Gateway", events,
				func(c *caches, l []cache.GenericLister) { c.gateways = append(c.gateways, l...) })
			go s.watchCRD(ctx, httpRouteGVR(), "Gateway API HTTPRoute", events,
				func(c *caches, l []cache.GenericLister) { c.httpRoutes = append(c.httpRoutes, l...) })
		}
	}

	var applied map[string]discovery.Container
	applied = s.reconcile(ctx, h, applied)
	live := make(map[string]bool, len(applied))
	for id := range applied {
		live[id] = true
	}
	h.Retain(live)
	s.log.Info(
		"kubernetes sync complete",
		zap.Int("services", len(applied)),
		zap.String("access", string(s.cfg.Access)),
	)

	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-trigger:
		}
		timer.Reset(s.cfg.Debounce)
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		select { // events that arrived while waiting are covered by this reconcile
		case <-trigger:
		default:
		}
		applied = s.reconcile(ctx, h, applied)
	}
}

// caches holds the listers; the CRD-based ones appear once the CRD exists.
type caches struct {
	services  []serviceLister
	slices    []sliceLister
	ingresses []ingressLister
	routes    []cache.GenericLister
	// Gateway API (optional CRDs): HTTPRoutes name the hosts, Gateways tell whether a listener is TLS.
	httpRoutes []cache.GenericLister
	gateways   []cache.GenericLister
}

type (
	serviceLister interface {
		List(selector labels.Selector) ([]*corev1.Service, error)
	}
	sliceLister interface {
		List(selector labels.Selector) ([]*discoveryv1.EndpointSlice, error)
	}
	ingressLister interface {
		List(selector labels.Selector) ([]*networkingv1.Ingress, error)
	}
)

func (s *Source) namespaces() []string {
	if len(s.cfg.Namespaces) == 0 {
		return []string{metav1.NamespaceAll}
	}
	return s.cfg.Namespaces
}

// startCore starts the typed informers and returns their HasSynced functions.
func (s *Source) startCore(ctx context.Context, events cache.ResourceEventHandler) []cache.InformerSynced {
	enable := discovery.EnableKey(s.cfg.LabelPrefix) + "=true"
	var synced []cache.InformerSynced
	var c caches
	for _, ns := range s.namespaces() {
		// Services are filtered server-side; the other kinds are needed unfiltered.
		svcFactory := informers.NewSharedInformerFactoryWithOptions(s.cl.Core, s.cfg.Resync,
			informers.WithNamespace(ns),
			informers.WithTweakListOptions(func(o *metav1.ListOptions) { o.LabelSelector = enable }))
		otherFactory := informers.NewSharedInformerFactoryWithOptions(
			s.cl.Core,
			s.cfg.Resync,
			informers.WithNamespace(ns),
		)

		svc := svcFactory.Core().V1().Services()
		eps := otherFactory.Discovery().V1().EndpointSlices()
		ing := otherFactory.Networking().V1().Ingresses()
		for _, inf := range []cache.SharedIndexInformer{svc.Informer(), eps.Informer(), ing.Informer()} {
			if _, err := inf.AddEventHandler(events); err != nil {
				s.log.Error("cannot register kubernetes event handler", zap.Error(err))
			}
			synced = append(synced, inf.HasSynced)
		}
		c.services = append(c.services, svc.Lister())
		c.slices = append(c.slices, eps.Lister())
		c.ingresses = append(c.ingresses, ing.Lister())
		svcFactory.Start(ctx.Done())
		otherFactory.Start(ctx.Done())
	}
	s.mu.Lock()
	s.caches = c
	s.mu.Unlock()
	return synced
}

func (s *Source) waitForSync(ctx context.Context, synced []cache.InformerSynced) {
	wctx, cancel := context.WithTimeout(ctx, s.cfg.SyncTimeout)
	defer cancel()
	if !cache.WaitForCacheSync(wctx.Done(), synced...) && ctx.Err() == nil {
		s.log.Warn("kubernetes caches not fully synced (missing RBAC?); continuing, informers keep retrying",
			zap.Duration("timeout", s.cfg.SyncTimeout))
	}
}

// reconcile recomputes the desired containers and reports the differences to h.
func (s *Source) reconcile(
	ctx context.Context,
	h discovery.Handler,
	prev map[string]discovery.Container,
) map[string]discovery.Container {
	desired := s.desired()
	for id, ctr := range desired {
		if prev[id] != ctr {
			h.Apply(ctx, ctr)
		}
	}
	for id := range prev {
		if _, ok := desired[id]; !ok {
			h.Remove(id)
		}
	}
	return desired
}

// desired builds the container for every documented Service from the caches.
func (s *Source) desired() map[string]discovery.Container {
	s.mu.RLock()
	c := s.caches
	s.mu.RUnlock()

	ready := readiness(c)
	var derived map[string]map[string]bool
	if s.cfg.DeriveServerURL {
		derived = publicURLs(c)
	}

	enableKey := discovery.EnableKey(s.cfg.LabelPrefix)
	out := map[string]discovery.Container{}
	for _, l := range c.services {
		svcs, err := l.List(labels.Everything())
		if err != nil {
			s.log.Warn("list services failed", zap.Error(err))
			continue
		}
		for _, svc := range svcs {
			if svc.Labels[enableKey] != "true" {
				continue // the server-side selector already filters; this also covers a removed label
			}
			key := svc.Namespace + "/" + svc.Name
			ctr := s.container(svc, ready[key])
			if ctr.ServerURL == "" && ctr.Problem == "" {
				if u, ok := discovery.SingleURL(derived[key]); ok {
					ctr.ServerURL = u
				}
			}
			out[ctr.ID] = ctr
		}
	}
	return out
}

// container turns one Service into a discovery.Container.
func (s *Source) container(svc *corev1.Service, ready bool) discovery.Container {
	prefix := s.cfg.LabelPrefix
	ctr := discovery.Container{
		ID:      string(svc.UID),
		Name:    svc.Name + "." + svc.Namespace,
		Source:  SourceName,
		Running: ready,
	}
	attrs := maps.Clone(svc.Labels)
	if attrs == nil {
		attrs = map[string]string{}
	}
	maps.Copy(attrs, svc.Annotations) // annotations carry the values labels cannot (URLs)

	// A named port is resolved to its number so ParseTarget only deals with numbers.
	if p := attrs[prefix+".port"]; p != "" {
		if _, err := strconv.ParseUint(p, 10, 16); err != nil {
			for _, sp := range svc.Spec.Ports {
				if sp.Name == p {
					attrs[prefix+".port"] = strconv.Itoa(int(sp.Port))
				}
			}
		}
	}
	var ports []uint16
	for _, sp := range svc.Spec.Ports {
		if sp.Protocol == "" || sp.Protocol == corev1.ProtocolTCP {
			ports = append(ports, uint16(sp.Port)) //nolint:gosec // port numbers are 1..65535 by API validation
		}
	}

	t, err := discovery.ParseTarget(prefix, attrs, []string{ctr.Name}, ports)
	if err != nil {
		ctr.Problem = err.Error()
		return ctr
	}
	ctr.Name, ctr.Group, ctr.Order, ctr.ServerURL, ctr.KeepServerPath = t.Name, t.Group, t.Order, t.ServerURL, t.KeepServerPath
	if !hasPort(svc, t.Port) {
		ctr.Problem = fmt.Sprintf("the Service does not expose port %d", t.Port)
		return ctr
	}
	ctr.BaseURL = s.baseURL(svc, t.Port, t.Scheme)
	ctr.SpecURL = ctr.BaseURL + t.Path
	return ctr
}

func hasPort(svc *corev1.Service, port uint16) bool {
	for _, sp := range svc.Spec.Ports {
		if sp.Port == int32(port) {
			return true
		}
	}
	return false
}

func (s *Source) baseURL(svc *corev1.Service, port uint16, scheme string) string {
	p := strconv.Itoa(int(port))
	if s.cfg.Access == AccessAPIServer {
		// The service proxy addresses https backends as "https:<name>:<port>".
		name := url.PathEscape(svc.Name) + ":" + p
		if scheme == discovery.SchemeHTTPS {
			name = "https:" + name
		}
		return strings.TrimRight(s.cfg.APIServer, "/") + "/api/v1/namespaces/" + url.PathEscape(svc.Namespace) +
			"/services/" + name + "/proxy"
	}
	return scheme + "://" + svc.Name + "." + svc.Namespace + ".svc." + s.cfg.ClusterDomain + ":" + p
}

// readiness reports, per "namespace/service", whether any endpoint is ready.
func readiness(c caches) map[string]bool {
	out := map[string]bool{}
	for _, l := range c.slices {
		slices, err := l.List(labels.Everything())
		if err != nil {
			continue
		}
		for _, sl := range slices {
			name := sl.Labels[discoveryv1.LabelServiceName]
			if name == "" {
				continue
			}
			for _, ep := range sl.Endpoints {
				if ep.Conditions.Ready == nil || *ep.Conditions.Ready {
					out[sl.Namespace+"/"+name] = true
				}
			}
		}
	}
	return out
}

// listUnstructured returns all objects of a GenericLister as unstructured maps.
func listUnstructured(l cache.GenericLister) []map[string]any {
	objs, err := l.List(labels.Everything())
	if err != nil {
		return nil
	}
	out := make([]map[string]any, 0, len(objs))
	for _, o := range objs {
		if u, ok := o.(runtime.Unstructured); ok {
			out = append(out, u.UnstructuredContent())
		}
	}
	return out
}
