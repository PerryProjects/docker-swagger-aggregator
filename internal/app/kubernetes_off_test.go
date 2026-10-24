//go:build nokubernetes

package app_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/perryprojects/docker-swagger-aggregator/internal/app"
)

// The small Docker-only build starts without --kubernetes and says clearly why --kubernetes cannot work.
func TestWithoutKubernetesSupportTheFlagIsRefusedClearly(t *testing.T) {
	t.Parallel()

	cfg := app.DefaultConfig()
	cfg.DockerEnabled = false
	cfg.Kubernetes.Enabled = true
	_, err := app.New(cfg, app.Deps{})
	var fe app.FieldError
	if !errors.As(err, &fe) || fe.Field != "kubernetes" || !strings.Contains(fe.Message, "no Kubernetes support") {
		t.Fatalf("got %v", err)
	}
}
