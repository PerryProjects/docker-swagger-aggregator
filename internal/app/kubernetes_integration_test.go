//go:build integration

package app_test

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"os"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	kube "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/perryprojects/docker-swagger-aggregator/internal/app"
)

// itContextEnv names the kubeconfig context the test may use. The test creates and deletes a
// namespace, so it never runs unless a context is named explicitly (never point it at production).
//
//	SWAGGER_AGGREGATOR_IT_KUBE_CONTEXT=docker-desktop \
//	  go test -tags=integration -count=1 -timeout 240s ./internal/app -run KubernetesIntegration
const (
	itContextEnv = "SWAGGER_AGGREGATOR_IT_KUBE_CONTEXT"
	itSpecJSON   = `{"openapi":"3.0.3","info":{"title":"it","version":"1"},"paths":{}}`
	itWait       = 150 * time.Second // the pod may have to pull busybox
)

// itCluster returns a client for the named context, or skips the test.
func itCluster(t *testing.T) (*kube.Clientset, string) {
	t.Helper()
	kubeContext := os.Getenv(itContextEnv)
	if kubeContext == "" {
		t.Skipf("set %s to a disposable cluster's context (e.g. docker-desktop) to run this", itContextEnv)
	}
	restCfg, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		clientcmd.NewDefaultClientConfigLoadingRules(),
		&clientcmd.ConfigOverrides{CurrentContext: kubeContext},
	).ClientConfig()
	if err != nil {
		t.Skipf("no usable kubeconfig context %q: %v", kubeContext, err)
	}
	cs, err := kube.NewForConfig(restCfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = cs.Discovery().ServerVersion(); err != nil {
		t.Skipf("cluster %q not reachable: %v", kubeContext, err)
	}
	return cs, kubeContext
}

// itFixture creates a namespace (deleted on cleanup) with a busybox pod serving a spec, a labeled
// Service in front of it and an Ingress that routes a TLS host to the Service.
func itFixture(t *testing.T, cs *kube.Clientset) string {
	t.Helper()
	ctx := t.Context()
	ns := fmt.Sprintf("sa-it-%x", time.Now().UnixNano()%0xfffffff)
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	_, err := cs.CoreV1().
		Namespaces().
		Create(ctx, &corev1.Namespace{Name: ns}, metav1.CreateOptions{})
	must(err)
	t.Cleanup(func() { _ = cs.CoreV1().Namespaces().Delete(context.WithoutCancel(ctx), ns, metav1.DeleteOptions{}) })

	_, err = cs.CoreV1().Pods(ns).Create(ctx, &corev1.Pod{
		Namespace: ns, Name: "httpd", Labels: map[string]string{"app": "items"},
		Spec: corev1.PodSpec{Containers: []corev1.Container{
			{
				Name:  "httpd",
				Image: "busybox:1.37",
				Command: []string{
					"sh",
					"-c",
					`mkdir -p /www && echo "$SPEC" > /www/openapi.json && echo '[{"id":1}]' > /www/items && exec httpd -f -p 8000 -h /www`,
				},
				Env: []corev1.EnvVar{{Name: "SPEC", Value: itSpecJSON}},
				ReadinessProbe: &corev1.Probe{
					HTTPGet:       &corev1.HTTPGetAction{Path: "/openapi.json", Port: intstr.FromInt32(8000)},
					PeriodSeconds: 1,
				},
			},
		}},
	}, metav1.CreateOptions{})
	must(err)
	_, err = cs.CoreV1().Services(ns).Create(ctx, &corev1.Service{
		Namespace: ns, Name: "items", Labels: map[string]string{"swagger-aggregator.enable": "true"},
		Spec: corev1.ServiceSpec{
			Selector: map[string]string{"app": "items"},
			Ports:    []corev1.ServicePort{{Name: "http", Port: 8080, TargetPort: intstr.FromInt32(8000)}},
		},
	}, metav1.CreateOptions{})
	must(err)
	pt := networkingv1.PathTypePrefix
	_, err = cs.NetworkingV1().Ingresses(ns).Create(ctx, &networkingv1.Ingress{
		Namespace: ns, Name: "items",
		Spec: networkingv1.IngressSpec{
			TLS: []networkingv1.IngressTLS{{Hosts: []string{"items.it.test"}}},
			Rules: []networkingv1.IngressRule{{Host: "items.it.test", HTTP: &networkingv1.HTTPIngressRuleValue{
				Paths: []networkingv1.HTTPIngressPath{{
					Path: "/", PathType: &pt,
					Backend: networkingv1.IngressBackend{Service: &networkingv1.IngressServiceBackend{
						Name: "items", Port: networkingv1.ServiceBackendPort{Number: 8080},
					}},
				}},
			}}},
		},
	}, metav1.CreateOptions{})
	must(err)
	return ns
}

func TestKubernetesIntegrationOutsideTheCluster(t *testing.T) {
	t.Parallel()

	cs, kubeContext := itCluster(t)
	ns := itFixture(t, cs)

	cfg := app.DefaultConfig()
	cfg.DockerEnabled = false
	cfg.DeriveServerURL = true
	cfg.ProbeInterval = time.Second
	cfg.ShutdownGrace = 200 * time.Millisecond
	cfg.Kubernetes = app.KubernetesConfig{
		Enabled:       true,
		Context:       kubeContext,
		Namespaces:    []string{ns},
		Access:        "apiserver",
		ClusterDomain: "cluster.local",
	}
	a, err := app.New(cfg, app.Deps{})
	if err != nil {
		t.Fatal(err)
	}
	ln, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	runCtx, cancel := context.WithCancel(t.Context())
	served := make(chan error, 1)
	go func() { served <- a.Serve(runCtx, ln) }()
	defer func() {
		cancel()
		<-served
	}()
	base := "http://" + ln.Addr().String()
	name := "items." + ns

	status := func() string {
		for _, s := range getJSON[[]svc](t, base+"/api/services") {
			if s.Name == name {
				return s.Status
			}
		}
		return ""
	}
	waitUntil := func(what, want string) {
		t.Helper()
		for end := time.Now().Add(itWait); time.Now().Before(end); time.Sleep(500 * time.Millisecond) {
			if status() == want {
				return
			}
		}
		t.Fatalf("%s: status is %q, want %q", what, status(), want)
	}
	waitUntil("service up", "up")

	// the public URL comes from the Ingress; Try-it for it is mapped to the Service through the API server proxy
	spec := getJSON[map[string]any](t, base+"/api/services/"+name+"/spec")
	servers, _ := spec["servers"].([]any)
	first, _ := servers[0].(map[string]any)
	if len(servers) != 1 || first["url"] != "https://items.it.test" {
		t.Fatalf("servers = %v", servers)
	}
	body := getJSON[[]map[string]int](
		t,
		base+"/api/scalar-proxy?scalar_url="+url.QueryEscape("https://items.it.test/items"),
	)
	if body[0]["id"] != 1 {
		t.Fatalf("proxied body %v", body)
	}

	// no ready endpoint -> down
	if err = cs.CoreV1().
		Pods(ns).
		Delete(t.Context(), "httpd", metav1.DeleteOptions{GracePeriodSeconds: new(int64)}); err != nil {
		t.Fatal(err)
	}
	waitUntil("service down after the pod is deleted", "down")
}
