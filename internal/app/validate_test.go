package app_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/perryprojects/docker-swagger-aggregator/internal/app"
)

func TestValidate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		mutate func(*app.Config)
		field  string // expected failing flag; "" = valid
		want   string // substring of the message
	}{
		{"defaults are valid", func(*app.Config) {}, "", ""},
		{"kubernetes only", func(c *app.Config) { c.DockerEnabled = false; c.Kubernetes.Enabled = true }, "", ""},
		{"docker and kubernetes together", func(c *app.Config) { c.Kubernetes.Enabled = true }, "", ""},
		{"full kubernetes options", func(c *app.Config) {
			c.Kubernetes = app.KubernetesConfig{
				Enabled: true, Kubeconfig: "/k", Context: "ctx", Access: "apiserver",
				ClusterDomain: "cluster.example.org", Namespaces: []string{"a", "b-2"},
			}
		}, "", ""},

		{"base path ok", func(c *app.Config) { c.BasePath = "/docs" }, "", ""},
		{"base path without slash", func(c *app.Config) { c.BasePath = "docs" }, "base-path", "leading slash"},
		{"base path trailing slash", func(c *app.Config) { c.BasePath = "/docs/" }, "base-path", "no trailing slash"},

		{"listen empty", func(c *app.Config) { c.Listen = "" }, "listen", "must not be empty"},
		{"listen without port", func(c *app.Config) { c.Listen = "localhost" }, "listen", "not host:port"},
		{"listen bad port", func(c *app.Config) { c.Listen = ":http" }, "listen", "not a number"},
		{"listen port too large", func(c *app.Config) { c.Listen = ":70000" }, "listen", "not a number"},
		{"listen port zero", func(c *app.Config) { c.Listen = ":0" }, "listen", "random port"},
		{"listen host and port", func(c *app.Config) { c.Listen = "127.0.0.1:9090" }, "", ""},

		{"prefix empty", func(c *app.Config) { c.LabelPrefix = "" }, "label-prefix", "must not be empty"},
		{
			"prefix with space",
			func(c *app.Config) { c.LabelPrefix = "my prefix" },
			"label-prefix",
			"not a valid label name",
		},
		{
			"prefix ends with dot",
			func(c *app.Config) { c.LabelPrefix = "docs." },
			"label-prefix",
			"not a valid label name",
		},
		{
			"prefix too long for a label",
			func(c *app.Config) { c.LabelPrefix = strings.Repeat("a", 60) },
			"label-prefix",
			"too long",
		},
		{"prefix dotted is fine", func(c *app.Config) { c.LabelPrefix = "staging.docs" }, "", ""},

		{
			"probe too small",
			func(c *app.Config) { c.ProbeInterval = time.Nanosecond },
			"probe-interval",
			"out of range",
		},
		{"probe zero", func(c *app.Config) { c.ProbeInterval = 0 }, "probe-interval", "out of range"},
		{"probe negative", func(c *app.Config) { c.ProbeInterval = -time.Second }, "probe-interval", "out of range"},
		{"probe too large", func(c *app.Config) { c.ProbeInterval = 48 * time.Hour }, "probe-interval", "out of range"},
		{"fetch timeout zero", func(c *app.Config) { c.FetchTimeout = 0 }, "fetch-timeout", "out of range"},
		{"fetch timeout huge", func(c *app.Config) { c.FetchTimeout = time.Hour }, "fetch-timeout", "out of range"},
		{"grace zero", func(c *app.Config) { c.ShutdownGrace = 0 }, "shutdown-grace", "out of range"},
		{"spec limit tiny", func(c *app.Config) { c.MaxSpecBytes = 10 }, "max-spec-bytes", "out of range"},
		{"spec limit negative", func(c *app.Config) { c.MaxSpecBytes = -1 }, "max-spec-bytes", "out of range"},
		{"spec limit huge", func(c *app.Config) { c.MaxSpecBytes = 1 << 40 }, "max-spec-bytes", "out of range"},

		{"no source", func(c *app.Config) { c.DockerEnabled = false }, "docker", "no source is enabled"},
		{"docker host scheme", func(c *app.Config) { c.DockerHost = "ftp://x" }, "docker-host", "must look like"},
		{
			"docker host without scheme",
			func(c *app.Config) { c.DockerHost = "socket-proxy:2375" },
			"docker-host",
			"must look like",
		},
		{"docker host tcp", func(c *app.Config) { c.DockerHost = "tcp://socket-proxy:2375" }, "", ""},
		{"docker network invalid", func(c *app.Config) { c.Network = "-bad" }, "network", "not a valid Docker network"},
		{"docker network ok", func(c *app.Config) { c.Network = "swagger-aggregator_default" }, "", ""},
		{"docker option while docker is off", func(c *app.Config) {
			c.DockerEnabled = false
			c.Kubernetes.Enabled = true
			c.Network = "x"
		}, "network", "has no effect because --docker is disabled"},

		{
			"kube option while kubernetes is off",
			func(c *app.Config) { c.Kubernetes.Namespaces = []string{"a"} },
			"namespace",
			"has no effect because --kubernetes is disabled",
		},
		{
			"kubeconfig while kubernetes is off",
			func(c *app.Config) { c.Kubernetes.Kubeconfig = "/k" },
			"kubeconfig",
			"has no effect",
		},
		{
			"bad kube access",
			func(c *app.Config) { c.Kubernetes.Enabled = true; c.Kubernetes.Access = "magic" },
			"kube-access",
			"not one of",
		},
		{
			"bad cluster domain",
			func(c *app.Config) { c.Kubernetes.Enabled = true; c.Kubernetes.ClusterDomain = "Cluster Local" },
			"cluster-domain",
			"not a valid DNS name",
		},
		{
			"bad namespace",
			func(c *app.Config) { c.Kubernetes.Enabled = true; c.Kubernetes.Namespaces = []string{"Upper_Case"} },
			"namespace",
			"not a valid namespace",
		},
		{
			"duplicate namespace",
			func(c *app.Config) { c.Kubernetes.Enabled = true; c.Kubernetes.Namespaces = []string{"a", "a"} },
			"namespace",
			"more than once",
		},
	}
	for _, tt := range tests {
		cfg := app.DefaultConfig()
		tt.mutate(&cfg)
		err := cfg.Validate()
		if tt.field == "" {
			if err != nil {
				t.Errorf("%s: unexpected error: %v", tt.name, err)
			}
			continue
		}
		var fe app.FieldError
		if !errors.As(err, &fe) {
			t.Errorf("%s: want a FieldError for --%s, got %v", tt.name, tt.field, err)
			continue
		}
		if fe.Field != tt.field || !strings.Contains(fe.Message, tt.want) {
			t.Errorf("%s: got --%s: %s, want --%s containing %q", tt.name, fe.Field, fe.Message, tt.field, tt.want)
		}
	}
}

func TestValidateReportsEveryProblemAtOnceInAStableOrder(t *testing.T) {
	t.Parallel()

	cfg := app.DefaultConfig()
	cfg.Listen = "nope"
	cfg.ProbeInterval = 0
	cfg.MaxSpecBytes = 1
	cfg.Network = "x"
	cfg.DockerEnabled = false
	cfg.Kubernetes.Namespaces = []string{"a"}

	var first string
	for range 20 { // map iteration order must not leak into the message
		err := cfg.Validate()
		ve, ok := errors.AsType[*app.ValidationError](err)
		if !ok {
			t.Fatalf("want *ValidationError, got %T", err)
		}
		if got := len(ve.Problems); got != 6 {
			t.Fatalf("want 6 problems, got %d: %v", got, err)
		}
		if first == "" {
			first = err.Error()
		} else if err.Error() != first {
			t.Fatalf("message order is not stable:\n%s\n---\n%s", first, err)
		}
	}
	for _, want := range []string{"6 problems", "--listen", "--probe-interval", "--max-spec-bytes", "--network", "--namespace", "--docker:"} {
		if !strings.Contains(first, want) {
			t.Errorf("message lacks %q:\n%s", want, first)
		}
	}
}

func TestNewRefusesInvalidConfig(t *testing.T) {
	t.Parallel()

	cfg := app.DefaultConfig()
	cfg.Listen = "nope"
	_, err := app.New(cfg, app.Deps{Docker: &fakeDocker{}})
	if _, ok := errors.AsType[*app.ValidationError](err); !ok {
		t.Fatalf("want a ValidationError, got %v", err)
	}
}
