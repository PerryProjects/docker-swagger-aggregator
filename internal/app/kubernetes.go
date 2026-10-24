//go:build !nokubernetes

// The Kubernetes source needs client-go and the Kubernetes API types, which are about 26 MB of the API binary. Builds
// without them (-tags nokubernetes, a Docker-only image) use kubernetes_off.go instead.

package app

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"go.uber.org/zap"
	"k8s.io/client-go/dynamic"
	kube "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/perryprojects/docker-swagger-aggregator/internal/discovery"
	"github.com/perryprojects/docker-swagger-aggregator/internal/discovery/kubernetes"
)

// KubernetesClients are the clients the Kubernetes source uses (Deps.Kubernetes).
type KubernetesClients = kubernetes.Clients

// newKubernetesSource builds the clients (unless injected), decides how services
// are reached and registers an authenticated transport for the API server host
// when going through its service proxy.
func newKubernetesSource(
	cfg Config,
	deps Deps,
	byHost map[string]http.RoundTripper,
	log *zap.Logger,
) (discovery.Source, error) {
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
