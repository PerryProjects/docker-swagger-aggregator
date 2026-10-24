//go:build nokubernetes

package app

import (
	"net/http"

	"go.uber.org/zap"

	"github.com/perryprojects/docker-swagger-aggregator/internal/discovery"
)

// KubernetesClients is an empty placeholder: this build has no Kubernetes support.
type KubernetesClients struct{}

// newKubernetesSource always fails: client-go is not part of this build (-tags nokubernetes).
func newKubernetesSource(Config, Deps, map[string]http.RoundTripper, *zap.Logger) (discovery.Source, error) {
	return nil, FieldError{
		Field: "kubernetes",
		Message: "this build has no Kubernetes support (built with -tags nokubernetes, the small Docker-only image); " +
			"use the full image, which is what `docker build .` makes",
	}
}
