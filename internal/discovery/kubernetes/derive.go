package kubernetes

import (
	"context"
	"slices"
	"strings"
	"time"

	"go.uber.org/zap"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic/dynamicinformer"
	"k8s.io/client-go/tools/cache"

	"github.com/perryprojects/docker-swagger-aggregator/internal/discovery"
)

// ingressRouteGVR is Traefik v3's CRD (v2 used traefik.containo.us, not handled).
func ingressRouteGVR() schema.GroupVersionResource {
	return schema.GroupVersionResource{Group: "traefik.io", Version: "v1alpha1", Resource: "ingressroutes"}
}

// publicURLs returns, per "namespace/service", the set of public base URLs that
// Ingress and IngressRoute objects route to it. Deliberately conservative, like
// the Traefik label derivation: only host-based routes with the root path count
// (a path prefix would need to know whether the controller strips it before the
// request reaches the Service), and wildcard hosts are ignored. The caller only
// uses a URL when exactly one results.
func publicURLs(c caches) map[string]map[string]bool {
	out := map[string]map[string]bool{}
	add := func(key, u string) {
		if out[key] == nil {
			out[key] = map[string]bool{}
		}
		out[key][u] = true
	}
	for _, l := range c.ingresses {
		ings, err := l.List(labels.Everything())
		if err != nil {
			continue
		}
		for _, ing := range ings {
			addIngress(ing, add)
		}
	}
	for _, l := range c.routes {
		for _, obj := range listUnstructured(l) {
			addIngressRoute(obj, add)
		}
	}
	return out
}

// addIngress reads the host rules of a standard Ingress.
func addIngress(ing *networkingv1.Ingress, add func(key, u string)) {
	for _, rule := range ing.Spec.Rules {
		if rule.Host == "" || rule.HTTP == nil || strings.Contains(rule.Host, "*") {
			continue
		}
		scheme := "http"
		for _, t := range ing.Spec.TLS {
			if len(t.Hosts) == 0 || slices.Contains(t.Hosts, rule.Host) {
				scheme = "https"
			}
		}
		for _, p := range rule.HTTP.Paths {
			if (p.Path != "" && p.Path != "/") || p.Backend.Service == nil {
				continue
			}
			add(ing.Namespace+"/"+p.Backend.Service.Name, scheme+"://"+rule.Host)
		}
	}
}

// addIngressRoute reads spec.routes[].{match,services[]} and spec.tls of one IngressRoute.
func addIngressRoute(obj map[string]any, add func(key, u string)) {
	ns, _, _ := unstructured.NestedString(obj, "metadata", "namespace")
	routes, _, _ := unstructured.NestedSlice(obj, "spec", "routes")
	_, hasTLS, _ := unstructured.NestedFieldNoCopy(obj, "spec", "tls")
	scheme := "http"
	if hasTLS {
		scheme = "https"
	}
	for _, r := range routes {
		route, ok := r.(map[string]any)
		if !ok {
			continue
		}
		match, _ := route["match"].(string)
		host, ok := discovery.HostOnlyRule(match)
		if !ok {
			continue
		}
		svcs, _ := route["services"].([]any)
		for _, sv := range svcs {
			if key, found := routeService(sv, ns); found {
				add(key, scheme+"://"+host)
			}
		}
	}
}

// routeService returns "namespace/name" for a plain Service reference of an IngressRoute.
func routeService(raw any, defaultNS string) (string, bool) {
	svc, ok := raw.(map[string]any)
	if !ok {
		return "", false
	}
	if kind, _ := svc["kind"].(string); kind != "" && kind != "Service" {
		return "", false // TraefikService etc.
	}
	name, _ := svc["name"].(string)
	ns, _ := svc["namespace"].(string)
	if ns == "" {
		ns = defaultNS
	}
	return ns + "/" + name, name != ""
}

// watchIngressRoutes starts the IngressRoute informers as soon as the CRD exists.
// Without the CRD the informer would fail forever, so it is looked up first and
// rechecked every minute.
func (s *Source) watchIngressRoutes(ctx context.Context, events cache.ResourceEventHandler) {
	gv := ingressRouteGVR().GroupVersion().String()
	timer := time.NewTimer(0)
	defer timer.Stop()
	logged := false
	for {
		if _, err := s.cl.Core.Discovery().ServerResourcesForGroupVersionWithContext(ctx, gv); err == nil {
			break
		}
		if !logged {
			s.log.Info("Traefik IngressRoute CRD not found; IngressRoute URLs are ignored until it appears",
				zap.String("groupVersion", gv))
			logged = true
		}
		timer.Reset(s.cfg.CRDRecheck)
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
	}

	var listers []cache.GenericLister
	for _, ns := range s.namespaces() {
		f := dynamicinformer.NewFilteredDynamicSharedInformerFactory(s.cl.Dynamic, s.cfg.Resync, ns, nil)
		inf := f.ForResource(ingressRouteGVR())
		if _, err := inf.Informer().AddEventHandler(events); err != nil {
			s.log.Error("cannot register IngressRoute event handler", zap.Error(err))
			continue
		}
		listers = append(listers, inf.Lister())
		f.Start(ctx.Done())
	}
	s.mu.Lock()
	s.caches.routes = append(s.caches.routes, listers...)
	s.mu.Unlock()
	s.log.Info("watching Traefik IngressRoutes")
}
