package kubernetes_test

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/perryprojects/docker-swagger-aggregator/internal/discovery/kubernetes"
)

const gwAPI = "gateway.networking.k8s.io/v1"

func (e *env) addGateway(gvr schema.GroupVersionResource, obj *unstructured.Unstructured) {
	e.t.Helper()
	if _, err := e.dyn.Resource(gvr).Namespace(obj.GetNamespace()).
		Create(context.Background(), obj, metav1.CreateOptions{}); err != nil {
		e.t.Fatal(err)
	}
}

func gateway(name string, listeners ...map[string]any) *unstructured.Unstructured {
	ls := make([]any, len(listeners))
	for i, l := range listeners {
		ls[i] = l
	}
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": gwAPI, "kind": "Gateway",
		"metadata": map[string]any{"namespace": testNS, "name": name},
		"spec":     map[string]any{"listeners": ls},
	}}
}

func httpRoute(name, host, gw, service string, matches ...any) *unstructured.Unstructured {
	rule := map[string]any{"backendRefs": []any{map[string]any{"name": service, "port": int64(8080)}}}
	if len(matches) > 0 {
		rule["matches"] = matches
	}
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": gwAPI, "kind": "HTTPRoute",
		"metadata": map[string]any{"namespace": testNS, "name": name},
		"spec": map[string]any{
			"hostnames":  []any{host},
			"parentRefs": []any{map[string]any{"name": gw}},
			"rules":      []any{rule},
		},
	}}
}

func TestServerURLDerivedFromGatewayAPIHTTPRoute(t *testing.T) {
	t.Parallel()

	e := start(t, kubernetes.Config{DeriveServerURL: true}, true,
		svc("ns", "secure", "u1", enabled(), nil),
		svc("ns", "plain", "u2", enabled(), nil),
		svc("ns", "prefix", "u3", enabled(), nil),
		svc("ns", "wild", "u4", enabled(), nil),
		svc("ns", "orphan", "u5", enabled(), nil),
	)
	waitFor(t, "services", func() bool { return e.rec.count() == 5 })

	gvrGW := schema.GroupVersionResource{Group: "gateway.networking.k8s.io", Version: "v1", Resource: "gateways"}
	gvrRT := schema.GroupVersionResource{Group: "gateway.networking.k8s.io", Version: "v1", Resource: "httproutes"}
	e.addGateway(gvrGW, gateway("tls-gw", map[string]any{"name": "https", "protocol": "HTTPS"}))
	e.addGateway(gvrGW, gateway("http-gw", map[string]any{"name": "http", "protocol": "HTTP"}))
	e.addGateway(gvrRT, httpRoute("r1", "secure.example.com", "tls-gw", "secure"))
	e.addGateway(gvrRT, httpRoute("r2", "plain.example.com", "http-gw", "plain"))
	e.addGateway(gvrRT, httpRoute("r3", "prefix.example.com", "tls-gw", "prefix",
		map[string]any{"path": map[string]any{"type": "PathPrefix", "value": "/v1"}}))
	e.addGateway(gvrRT, httpRoute("r4", "*.example.com", "tls-gw", "wild"))
	e.addGateway(gvrRT, httpRoute("r5", "orphan.example.com", "missing-gw", "orphan",
		map[string]any{"path": map[string]any{"type": "PathPrefix", "value": "/"}}))

	for id, want := range map[string]string{
		"u1": "https://secure.example.com", // listener terminates TLS
		"u2": "http://plain.example.com",   // HTTP listener
		"u3": "",                           // path prefix is ambiguous
		"u4": "",                           // wildcard host
		"u5": "http://orphan.example.com",  // unknown Gateway: no TLS known; PathPrefix "/" is the whole host
	} {
		waitFor(t, id, func() bool { c, _ := e.rec.get(id); return c.ServerURL == want })
	}
}
