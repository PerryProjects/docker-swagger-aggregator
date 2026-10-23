package docker

import (
	"strings"

	"github.com/perryprojects/docker-swagger-aggregator/internal/discovery"
)

const (
	traefikRouterPrefix = "traefik.http.routers."
	traefikRuleSuffix   = ".rule"
)

// TraefikServerURL derives the public base URL from Traefik router labels.
// Deliberately conservative; it gives up (false) unless exactly one public URL
// results:
//   - only routers whose rule is exactly Host(`x`) (a PathPrefix would need to
//     know whether a StripPrefix middleware removes it before the request
//     reaches the container);
//   - only routers with TLS configured through labels (`.tls=true` or `.tls.*`),
//     so the scheme is https; entrypoint-level TLS in the static config is
//     invisible here.
func TraefikServerURL(labels map[string]string) (string, bool) {
	urls := map[string]bool{}
	for key, rule := range labels {
		router, found := strings.CutPrefix(key, traefikRouterPrefix)
		if !found {
			continue
		}
		router, found = strings.CutSuffix(router, traefikRuleSuffix)
		if !found || strings.Contains(router, ".") {
			continue
		}
		host, ok := discovery.HostOnlyRule(rule)
		if !ok || !routerHasTLS(labels, router) {
			continue
		}
		urls["https://"+host] = true
	}
	return discovery.SingleURL(urls)
}

func routerHasTLS(labels map[string]string, router string) bool {
	prefix := traefikRouterPrefix + router + ".tls"
	if labels[prefix] == "true" {
		return true
	}
	for key := range labels {
		if strings.HasPrefix(key, prefix+".") {
			return true
		}
	}
	return false
}
