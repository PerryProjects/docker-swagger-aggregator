package kubernetes_test

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/zap"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	kubediscovery "k8s.io/client-go/discovery"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/perryprojects/docker-swagger-aggregator/internal/discovery"
	"github.com/perryprojects/docker-swagger-aggregator/internal/discovery/kubernetes"
)

const (
	prefix = discovery.DefaultPrefix
	testNS = "ns" // namespace of the Ingress/IngressRoute helpers
)

// recorder is a discovery.Handler that remembers the latest state per ID.
type recorder struct {
	mu      sync.Mutex
	applied map[string]discovery.Container
	applies int
}

func newRecorder() *recorder { return &recorder{applied: map[string]discovery.Container{}} }

func (r *recorder) Apply(_ context.Context, c discovery.Container) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.applied[c.ID] = c
	r.applies++
}

func (r *recorder) Remove(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.applied, id)
}

func (*recorder) Retain(map[string]bool) {}

func (r *recorder) get(id string) (discovery.Container, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	c, ok := r.applied[id]
	return c, ok
}

func (r *recorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.applied)
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for end := time.Now().Add(3 * time.Second); time.Now().Before(end); time.Sleep(5 * time.Millisecond) {
		if cond() {
			return
		}
	}
	t.Fatal("not met in 3s: ", what)
}

// env is a running Source against fake clients.
type env struct {
	t    *testing.T
	core *fake.Clientset
	dyn  *dynamicfake.FakeDynamicClient
	rec  *recorder
	crd  *atomic.Bool // whether the IngressRoute CRD "exists"
}

// crdDiscovery answers the IngressRoute group/version from an atomic flag; the
// fake discovery client is not safe to modify while it is being read.
type crdDiscovery struct {
	kubediscovery.DiscoveryInterfaces

	present *atomic.Bool
}

func (d crdDiscovery) ServerResourcesForGroupVersionWithContext(
	_ context.Context,
	gv string,
) (*metav1.APIResourceList, error) {
	if gv == "traefik.io/v1alpha1" && d.present.Load() {
		return &metav1.APIResourceList{GroupVersion: gv}, nil
	}
	return nil, apierrors.NewNotFound(schema.GroupResource{Group: "traefik.io", Resource: "ingressroutes"}, gv)
}

// coreClient is the fake clientset with the CRD-aware discovery client.
type coreClient struct {
	*fake.Clientset

	disc crdDiscovery
}

func (c coreClient) Discovery() kubediscovery.DiscoveryInterfaces { return c.disc }

func start(t *testing.T, cfg kubernetes.Config, withIngressRouteCRD bool, objs ...runtime.Object) *env {
	t.Helper()
	core := fake.NewClientset(objs...)
	scheme := runtime.NewScheme()
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme, map[schema.GroupVersionResource]string{
		{Group: "traefik.io", Version: "v1alpha1", Resource: "ingressroutes"}: "IngressRouteList",
	})
	crd := &atomic.Bool{}
	crd.Store(withIngressRouteCRD)
	cfg.Debounce = 5 * time.Millisecond
	cfg.CRDRecheck = 20 * time.Millisecond
	rec := newRecorder()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		kubernetes.New(kubernetes.Clients{Core: coreClient{Clientset: core, disc: crdDiscovery{DiscoveryInterfaces: core.Discovery(), present: crd}}, Dynamic: dyn}, cfg, zap.NewNop()).
			Run(ctx, rec)
		close(done)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Error("Run did not stop after cancel")
		}
	})
	return &env{t: t, core: core, dyn: dyn, rec: rec, crd: crd}
}

func svc(ns, name, uid string, lbl, ann map[string]string, ports ...corev1.ServicePort) *corev1.Service {
	if len(ports) == 0 {
		ports = []corev1.ServicePort{
			{Name: "http", Port: 8080, Protocol: corev1.ProtocolTCP, TargetPort: intstr.FromInt32(80)},
		}
	}
	return &corev1.Service{
		Namespace: ns, Name: name, UID: types.UID(uid), Labels: lbl, Annotations: ann,
		Spec: corev1.ServiceSpec{Ports: ports},
	}
}

func enabled() map[string]string { return map[string]string{prefix + ".enable": "true"} }

func slice(ns, service string, ready ...*bool) *discoveryv1.EndpointSlice {
	sl := &discoveryv1.EndpointSlice{
		Namespace: ns,
		Name:      service + "-abc",
		Labels:    map[string]string{discoveryv1.LabelServiceName: service},
	}
	for _, r := range ready {
		sl.Endpoints = append(
			sl.Endpoints,
			discoveryv1.Endpoint{Addresses: []string{"10.0.0.1"}, Conditions: discoveryv1.EndpointConditions{Ready: r}},
		)
	}
	return sl
}

func ingress(name, host, service string, tls bool, path string) *networkingv1.Ingress {
	pt := networkingv1.PathTypePrefix
	ing := &networkingv1.Ingress{
		Namespace: testNS, Name: name,
		Spec: networkingv1.IngressSpec{
			Rules: []networkingv1.IngressRule{{Host: host, HTTP: &networkingv1.HTTPIngressRuleValue{
				Paths: []networkingv1.HTTPIngressPath{{
					Path: path, PathType: &pt,
					Backend: networkingv1.IngressBackend{Service: &networkingv1.IngressServiceBackend{Name: service}},
				}},
			}}},
		},
	}
	if tls {
		ing.Spec.TLS = []networkingv1.IngressTLS{{Hosts: []string{host}}}
	}
	return ing
}

func ingressRoute(name, match, service string, tls bool) *unstructured.Unstructured {
	spec := map[string]any{"routes": []any{map[string]any{
		"match":    match,
		"services": []any{map[string]any{"name": service, "port": int64(8080)}},
	}}}
	if tls {
		spec["tls"] = map[string]any{}
	}
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "traefik.io/v1alpha1", "kind": "IngressRoute",
		"metadata": map[string]any{"namespace": testNS, "name": name},
		"spec":     spec,
	}}
}
