//go:build !nokubernetes

package app_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/perryprojects/docker-swagger-aggregator/internal/app"
)

func TestServiceAccessOutsideAClusterIsRefused(t *testing.T) {
	t.Parallel()

	cfg := app.DefaultConfig()
	cfg.DockerEnabled = false
	cfg.Kubernetes.Enabled = true
	cfg.Kubernetes.Access = "service"
	cfg.Kubernetes.Kubeconfig = "testdata/kubeconfig.yaml" // a valid file, so only the access mode is wrong
	_, err := app.New(cfg, app.Deps{})
	var fe app.FieldError
	if !errors.As(err, &fe) || fe.Field != "kube-access" || !strings.Contains(fe.Message, "inside the cluster") {
		t.Fatalf("got %v", err)
	}
}
