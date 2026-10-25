package kubernetes

import (
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/perryprojects/docker-swagger-aggregator/internal/discovery"
)

const gatewayGroup = "gateway.networking.k8s.io"

// Gateway API v1 (GA): HTTPRoute and Gateway.
func httpRouteGVR() schema.GroupVersionResource {
	return schema.GroupVersionResource{Group: gatewayGroup, Version: "v1", Resource: "httproutes"}
}

func gatewayGVR() schema.GroupVersionResource {
	return schema.GroupVersionResource{Group: gatewayGroup, Version: "v1", Resource: "gateways"}
}

// listener is the part of a Gateway listener that decides the public scheme.
type listener struct {
	name, protocol, hostname string
}

// gatewayListeners indexes the listeners of all Gateways by "namespace/name".
func gatewayListeners(c caches) map[string][]listener {
	out := map[string][]listener{}
	for _, l := range c.gateways {
		for _, obj := range listUnstructured(l) {
			ns, _, _ := unstructured.NestedString(obj, "metadata", "namespace")
			name, _, _ := unstructured.NestedString(obj, "metadata", "name")
			raw, _, _ := unstructured.NestedSlice(obj, "spec", "listeners")
			for _, r := range raw {
				m, ok := r.(map[string]any)
				if !ok {
					continue
				}
				ln := listener{}
				ln.name, _ = m["name"].(string)
				ln.protocol, _ = m["protocol"].(string)
				ln.hostname, _ = m["hostname"].(string)
				out[ns+"/"+name] = append(out[ns+"/"+name], ln)
			}
		}
	}
	return out
}

// addHTTPRoute reads one HTTPRoute. Like the Ingress derivation it is conservative: only routes with
// explicit, non-wildcard hostnames and a rule that matches the whole host (no path prefix, header, query
// or method restriction) count, and only plain Service backends.
func addHTTPRoute(obj map[string]any, gateways map[string][]listener, add func(key, u string)) {
	ns, _, _ := unstructured.NestedString(obj, "metadata", "namespace")
	hostnames, _, _ := unstructured.NestedStringSlice(obj, "spec", "hostnames")
	rules, _, _ := unstructured.NestedSlice(obj, "spec", "rules")
	parents, _, _ := unstructured.NestedSlice(obj, "spec", "parentRefs")
	for _, host := range hostnames {
		if host == "" || strings.Contains(host, "*") {
			continue
		}
		scheme := routeScheme(host, ns, parents, gateways)
		for _, r := range rules {
			rule, ok := r.(map[string]any)
			if !ok || !matchesWholeHost(rule) {
				continue
			}
			refs, _ := rule["backendRefs"].([]any)
			for _, ref := range refs {
				if key, found := backendService(ref, ns); found {
					add(key, scheme+"://"+host)
				}
			}
		}
	}
}

// matchesWholeHost reports whether the rule applies to every request of the host: no matches at all, or a
// match that is at most PathPrefix "/".
func matchesWholeHost(rule map[string]any) bool {
	matches, _ := rule["matches"].([]any)
	if len(matches) == 0 {
		return true
	}
	for _, m := range matches {
		match, ok := m.(map[string]any)
		if !ok {
			continue
		}
		if restricted(match) {
			continue
		}
		path, _ := match["path"].(map[string]any)
		if path == nil {
			return true
		}
		if t, _ := path["type"].(string); (t == "" || t == "PathPrefix") &&
			(path["value"] == nil || path["value"] == "/") {
			return true
		}
	}
	return false
}

// restricted reports whether a match narrows the request by header, query parameter or method.
func restricted(match map[string]any) bool {
	headers, _ := match["headers"].([]any)
	queryParams, _ := match["queryParams"].([]any)
	return len(headers) > 0 || len(queryParams) > 0 || match["method"] != nil
}

// backendService returns "namespace/name" for a plain Service backendRef with a non-zero weight.
func backendService(raw any, defaultNS string) (string, bool) {
	ref, ok := raw.(map[string]any)
	if !ok {
		return "", false
	}
	if kind, _ := ref["kind"].(string); kind != "" && kind != "Service" {
		return "", false
	}
	if group, _ := ref["group"].(string); group != "" {
		return "", false
	}
	if w, isInt := ref["weight"].(int64); isInt && w == 0 {
		return "", false
	}
	name, _ := ref["name"].(string)
	ns, _ := ref["namespace"].(string)
	if ns == "" {
		ns = defaultNS
	}
	return ns + "/" + name, name != ""
}

// routeScheme is https when a parent Gateway listener that serves the host terminates TLS.
func routeScheme(host, routeNS string, parents []any, gateways map[string][]listener) string {
	for _, p := range parents {
		ref, ok := p.(map[string]any)
		if !ok {
			continue
		}
		if kind, _ := ref["kind"].(string); kind != "" && kind != "Gateway" {
			continue
		}
		name, _ := ref["name"].(string)
		ns, _ := ref["namespace"].(string)
		if ns == "" {
			ns = routeNS
		}
		section, _ := ref["sectionName"].(string)
		for _, l := range gateways[ns+"/"+name] {
			if (section == "" || section == l.name) && l.protocol == "HTTPS" && listenerServes(l.hostname, host) {
				return discovery.SchemeHTTPS
			}
		}
	}
	return discovery.SchemeHTTP
}

// listenerServes reports whether a listener hostname (empty, exact or "*.suffix") covers host.
func listenerServes(listenerHost, host string) bool {
	if listenerHost == "" || listenerHost == host {
		return true
	}
	suffix, wild := strings.CutPrefix(listenerHost, "*")
	return wild && strings.HasSuffix(host, suffix) && len(host) > len(suffix)
}
