//go:build !nokubernetes

package app //nolint:testpackage // compares unexported constants of validate.go

import (
	"testing"

	"github.com/perryprojects/docker-swagger-aggregator/internal/discovery/kubernetes"
)

// validate.go compares --kube-access with local strings so that it does not import the Kubernetes source; they
// must stay equal to the source's own values.
func TestAccessConstantsMatchTheKubernetesSource(t *testing.T) {
	t.Parallel()
	if accessService != string(kubernetes.AccessService) || accessAPIServer != string(kubernetes.AccessAPIServer) {
		t.Fatalf("service=%q apiserver=%q, the source has %q and %q",
			accessService, accessAPIServer, kubernetes.AccessService, kubernetes.AccessAPIServer)
	}
}
