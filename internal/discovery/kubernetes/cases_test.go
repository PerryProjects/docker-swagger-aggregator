package kubernetes_test

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/perryprojects/docker-swagger-aggregator/internal/discovery"
	"github.com/perryprojects/docker-swagger-aggregator/internal/discovery/kubernetes"
)

func routeGVR() schema.GroupVersionResource {
	return schema.GroupVersionResource{Group: "traefik.io", Version: "v1alpha1", Resource: "ingressroutes"}
}

func (e *env) addRoute(obj *unstructured.Unstructured) {
	e.t.Helper()
	ns := obj.GetNamespace()
	if _, err := e.dyn.Resource(routeGVR()).
		Namespace(ns).
		Create(context.Background(), obj, metav1.CreateOptions{}); err != nil {
		e.t.Fatal(err)
	}
}

func TestServiceBecomesContainerInServiceMode(t *testing.T) {
	t.Parallel()

	e := start(t, kubernetes.Config{Access: kubernetes.AccessService}, false,
		svc("shop", "orders", "u1", enabled(), map[string]string{
			prefix + ".path": "docs/spec.json", prefix + ".group": "shop", prefix + ".order": "2",
		}),
		slice("shop", "orders", new(true)),
	)
	waitFor(t, "container", func() bool { c, ok := e.rec.get("u1"); return ok && c.Running })
	c, _ := e.rec.get("u1")
	base := "http://orders.shop.svc.cluster.local:8080"
	if c.Name != "orders.shop" || c.BaseURL != base || c.SpecURL != base+"/docs/spec.json" ||
		c.Group != "shop" || c.Order != 2 || c.Problem != "" || c.ServerURL != "" {
		t.Fatalf("got %+v", c)
	}
}

func TestOnlyLabeledServicesInWatchedNamespacesAreDocumented(t *testing.T) {
	t.Parallel()

	e := start(t, kubernetes.Config{Namespaces: []string{"a"}}, false,
		svc("a", "yes", "u1", enabled(), nil),
		svc("a", "unlabeled", "u2", map[string]string{"app": "x"}, nil),
		svc("a", "off", "u3", map[string]string{prefix + ".enable": "false"}, nil),
		svc("b", "other-namespace", "u4", enabled(), nil),
	)
	waitFor(t, "first", func() bool { _, ok := e.rec.get("u1"); return ok })
	for _, id := range []string{"u2", "u3", "u4"} {
		if _, ok := e.rec.get(id); ok {
			t.Errorf("%s must not be documented", id)
		}
	}
	// a service labeled later, in a watched namespace, appears; one in another namespace still does not
	if _, err := e.core.CoreV1().
		Services("a").
		Create(context.Background(), svc("a", "later", "u5", enabled(), nil), metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "later", func() bool { _, ok := e.rec.get("u5"); return ok })
}

func TestReadinessFollowsEndpointSlices(t *testing.T) {
	t.Parallel()

	e := start(t, kubernetes.Config{}, false,
		svc("ns", "api", "u1", enabled(), nil),
		svc("ns", "starting", "u2", enabled(), nil),
		svc("ns", "legacy", "u3", enabled(), nil),
		slice("ns", "starting", new(false)),
		slice("ns", "legacy", nil), // no ready condition = ready
	)
	waitFor(t, "all services", func() bool { return e.rec.count() == 3 })
	for id, want := range map[string]bool{"u1": false, "u2": false, "u3": true} {
		if c, _ := e.rec.get(id); c.Running != want {
			t.Errorf("%s: Running=%v, want %v", id, c.Running, want)
		}
	}

	// an endpoint becomes ready
	sl := slice("ns", "api", new(true))
	if _, err := e.core.DiscoveryV1().
		EndpointSlices("ns").
		Create(context.Background(), sl, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "api ready", func() bool { c, _ := e.rec.get("u1"); return c.Running })

	// and goes away again (scale to zero)
	if err := e.core.DiscoveryV1().
		EndpointSlices("ns").
		Delete(context.Background(), sl.Name, metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "api not ready", func() bool { c, _ := e.rec.get("u1"); return !c.Running })
}

func TestPortSelectionAndProblems(t *testing.T) {
	t.Parallel()

	two := []corev1.ServicePort{
		{Name: "metrics", Port: 9090, Protocol: corev1.ProtocolTCP},
		{Name: "http", Port: 8080, Protocol: corev1.ProtocolTCP},
	}
	udp := corev1.ServicePort{Name: "dns", Port: 53, Protocol: corev1.ProtocolUDP}
	e := start(
		t,
		kubernetes.Config{},
		false,
		svc(
			"ns",
			"first-port",
			"u1",
			enabled(),
			nil,
			two...), // default: first TCP port in spec order
		svc("ns", "named", "u2", enabled(), map[string]string{prefix + ".port": "http"}, two...), // port name -> number
		svc("ns", "numeric", "u3", enabled(), map[string]string{prefix + ".port": "9090"}, two...),
		svc("ns", "missing", "u4", enabled(), map[string]string{prefix + ".port": "1234"}, two...),
		svc("ns", "udp-only", "u5", enabled(), nil, udp),
		svc("ns", "bad-order", "u6", enabled(), map[string]string{prefix + ".order": "x"}),
		svc("ns", "bad-url", "u7", enabled(), map[string]string{prefix + ".server-url": "ftp://x"}),
	)
	waitFor(t, "all", func() bool { return e.rec.count() == 7 })
	port := func(id string) string { c, _ := e.rec.get(id); return c.BaseURL }
	if got := port("u1"); got != "http://first-port.ns.svc.cluster.local:9090" {
		t.Errorf("default port: %s", got)
	}
	if got := port("u2"); got != "http://named.ns.svc.cluster.local:8080" {
		t.Errorf("named port: %s", got)
	}
	if got := port("u3"); got != "http://numeric.ns.svc.cluster.local:9090" {
		t.Errorf("numeric port: %s", got)
	}
	for _, id := range []string{"u4", "u5", "u6", "u7"} {
		if c, _ := e.rec.get(id); c.Problem == "" || c.SpecURL != "" {
			t.Errorf("%s must be reported as a problem without a URL: %+v", id, c)
		}
	}
}

func TestAPIServerAccessUsesTheServiceProxy(t *testing.T) {
	t.Parallel()

	e := start(t, kubernetes.Config{Access: kubernetes.AccessAPIServer, APIServer: "https://127.0.0.1:6443/"}, false,
		svc("shop", "orders", "u1", enabled(), nil),
	)
	waitFor(t, "container", func() bool { _, ok := e.rec.get("u1"); return ok })
	c, _ := e.rec.get("u1")
	base := "https://127.0.0.1:6443/api/v1/namespaces/shop/services/orders:8080/proxy"
	if c.BaseURL != base || c.SpecURL != base+"/openapi.json" {
		t.Fatalf("got %+v", c)
	}
}

func TestServiceRemovalAndRelabeling(t *testing.T) {
	t.Parallel()

	e := start(t, kubernetes.Config{}, false,
		svc("ns", "a", "u1", enabled(), nil),
		svc("ns", "b", "u2", enabled(), nil),
	)
	waitFor(t, "both", func() bool { return e.rec.count() == 2 })

	if err := e.core.CoreV1().Services("ns").Delete(context.Background(), "a", metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "a removed", func() bool { _, ok := e.rec.get("u1"); return !ok })

	b := svc("ns", "b", "u2", map[string]string{"app": "b"}, nil) // label removed
	if _, err := e.core.CoreV1().Services("ns").Update(context.Background(), b, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "b removed", func() bool { _, ok := e.rec.get("u2"); return !ok })
}

func TestServerURLDerivedFromIngress(t *testing.T) {
	t.Parallel()

	e := start(
		t,
		kubernetes.Config{DeriveServerURL: true},
		false,
		svc("ns", "tls", "u1", enabled(), nil),
		svc("ns", "plain", "u2", enabled(), nil),
		svc("ns", "prefix", "u3", enabled(), nil),
		svc("ns", "wild", "u4", enabled(), nil),
		svc("ns", "two", "u5", enabled(), nil),
		svc(
			"ns",
			"explicit",
			"u6",
			enabled(),
			map[string]string{prefix + ".server-url": "https://explicit.example.com"},
		),
		ingress("i1", "tls.example.com", "tls", true, "/"),
		ingress("i2", "plain.example.com", "plain", false, ""),
		ingress("i3", "prefix.example.com", "prefix", true, "/v1"), // path prefix: ambiguous
		ingress("i4", "*.example.com", "wild", true, "/"),
		ingress("i5", "one.example.com", "two", true, "/"),
		ingress("i6", "two.example.com", "two", true, "/"), // two different hosts: ambiguous
		ingress("i7", "ignored.example.com", "explicit", true, "/"),
	)
	waitFor(t, "all", func() bool { return e.rec.count() == 6 })
	// ingress objects are listed by the informer shortly after the services; wait for the derived URLs
	waitFor(t, "derived", func() bool { c, _ := e.rec.get("u1"); return c.ServerURL != "" })
	for id, want := range map[string]string{
		"u1": "https://tls.example.com",
		"u2": "http://plain.example.com",
		"u3": "", "u4": "", "u5": "",
		"u6": "https://explicit.example.com", // an explicit annotation always wins
	} {
		if c, _ := e.rec.get(id); c.ServerURL != want {
			t.Errorf("%s: ServerURL = %q, want %q", id, c.ServerURL, want)
		}
	}
}

func TestNothingIsDerivedWithoutTheFlag(t *testing.T) {
	t.Parallel()

	e := start(t, kubernetes.Config{}, false,
		svc("ns", "api", "u1", enabled(), nil),
		ingress("i", "api.example.com", "api", true, "/"),
	)
	waitFor(t, "container", func() bool { _, ok := e.rec.get("u1"); return ok })
	if c, _ := e.rec.get("u1"); c.ServerURL != "" {
		t.Fatalf("derived without the flag: %+v", c)
	}
}

func TestServerURLDerivedFromTraefikIngressRoute(t *testing.T) {
	t.Parallel()

	e := start(t, kubernetes.Config{DeriveServerURL: true}, true,
		svc("ns", "api", "u1", enabled(), nil),
		svc("ns", "plain", "u2", enabled(), nil),
		svc("ns", "prefix", "u3", enabled(), nil),
		svc("other", "cross", "u4", enabled(), nil),
	)
	waitFor(t, "services", func() bool { return e.rec.count() == 4 })
	e.addRoute(ingressRoute("r1", "Host(`api.example.com`)", "api", true))
	e.addRoute(ingressRoute("r2", "Host(`plain.example.com`)", "plain", false))
	e.addRoute(ingressRoute("r3", "Host(`prefix.example.com`) && PathPrefix(`/v1`)", "prefix", true))
	cross := ingressRoute("r4", "Host(`cross.example.com`)", "cross", true)
	routes, _, _ := unstructured.NestedSlice(cross.Object, "spec", "routes")
	routes[0].(map[string]any)["services"].([]any)[0].(map[string]any)["namespace"] = "other" //nolint:forcetypeassert // test data
	_ = unstructured.SetNestedSlice(cross.Object, routes, "spec", "routes")
	e.addRoute(cross)

	waitFor(t, "derived", func() bool { c, _ := e.rec.get("u1"); return c.ServerURL == "https://api.example.com" })
	for id, want := range map[string]string{
		"u2": "http://plain.example.com", // no spec.tls
		"u3": "",                         // PathPrefix is ambiguous
		"u4": "https://cross.example.com",
	} {
		waitFor(t, id, func() bool { c, _ := e.rec.get(id); return c.ServerURL == want })
	}
}

func TestIngressRouteCRDAppearingLaterIsPickedUp(t *testing.T) {
	t.Parallel()

	e := start(t, kubernetes.Config{DeriveServerURL: true}, false, // CRD absent at start
		svc("ns", "api", "u1", enabled(), nil),
	)
	waitFor(t, "container", func() bool { _, ok := e.rec.get("u1"); return ok })
	if c, _ := e.rec.get("u1"); c.ServerURL != "" {
		t.Fatalf("no CRD, no URL: %+v", c)
	}
	// install the CRD (the fake discovery learns the group/version), then create a route
	e.crd.Store(true)
	e.addRoute(ingressRoute("r", "Host(`api.example.com`)", "api", true))
	waitFor(
		t,
		"derived after CRD",
		func() bool { c, _ := e.rec.get("u1"); return c.ServerURL == "https://api.example.com" },
	)
}

func TestUnchangedStateIsNotReappliedAndBurstsAreCoalesced(t *testing.T) {
	t.Parallel()

	e := start(t, kubernetes.Config{}, false, svc("ns", "api", "u1", enabled(), nil))
	waitFor(t, "container", func() bool { _, ok := e.rec.get("u1"); return ok })
	before := func() int { e.rec.mu.Lock(); defer e.rec.mu.Unlock(); return e.rec.applies }()
	// unrelated objects change; the documented container is identical -> no new Apply
	for _, name := range []string{"x", "y", "z"} {
		if _, err := e.core.CoreV1().
			Services("ns").
			Create(context.Background(), svc("ns", name, "n-"+name, map[string]string{"app": name}, nil), metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	time.Sleep(100 * time.Millisecond)
	if after := func() int { e.rec.mu.Lock(); defer e.rec.mu.Unlock(); return e.rec.applies }(); after != before {
		t.Fatalf("applied %d times after unrelated changes", after-before)
	}
}

var _ discovery.Handler = (*recorder)(nil)
