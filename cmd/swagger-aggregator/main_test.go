package main

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/spf13/pflag"

	"github.com/perryprojects/docker-swagger-aggregator/internal/app"
)

// load builds the configuration exactly as the program does: flags first, environment on top.
// These tests use t.Setenv, so they cannot run in parallel.
func load(t *testing.T, args ...string) (app.Config, error) {
	t.Helper()
	v := newViper()
	fs := pflag.NewFlagSet("test", pflag.ContinueOnError)
	registerFlags(fs, app.DefaultConfig())
	if err := fs.Parse(args); err != nil {
		t.Fatal(err)
	}
	if err := v.BindPFlags(fs); err != nil {
		t.Fatal(err)
	}
	return loadConfig(v)
}

func problems(t *testing.T, err error) []app.FieldError {
	t.Helper()
	var ve *app.ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("want *app.ValidationError, got %T: %v", err, err)
	}
	return ve.Problems
}

func TestDefaultsLoadAndValidate(t *testing.T) {
	t.Parallel()

	cfg, err := load(t)
	if err != nil {
		t.Fatal(err)
	}
	if def := app.DefaultConfig(); cfg.Listen != def.Listen || cfg.ProbeInterval != def.ProbeInterval ||
		!cfg.DockerEnabled || cfg.Kubernetes.Enabled || cfg.Kubernetes.Access != "auto" {
		t.Fatalf("defaults differ from app.DefaultConfig: %+v", cfg)
	}
}

func TestFlagsAndEnvironmentAreRead(t *testing.T) {
	t.Setenv("SWAGGER_AGGREGATOR_PROBE_INTERVAL", "2m")
	t.Setenv("SWAGGER_AGGREGATOR_KUBERNETES", "true")
	t.Setenv("SWAGGER_AGGREGATOR_NAMESPACE", "team-a, team-b")
	cfg, err := load(t, "--docker=false", "--listen", "127.0.0.1:9000", "--kube-access", "apiserver")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ProbeInterval != 2*time.Minute || !cfg.Kubernetes.Enabled || cfg.DockerEnabled ||
		cfg.Listen != "127.0.0.1:9000" || cfg.Kubernetes.Access != "apiserver" {
		t.Fatalf("got %+v", cfg)
	}
	if got := cfg.Kubernetes.Namespaces; len(got) != 2 || got[0] != "team-a" || got[1] != "team-b" {
		t.Fatalf("comma separated namespaces: %v", got)
	}
}

func TestUnparsableEnvironmentValuesAreReportedNotSilentlyZeroed(t *testing.T) {
	t.Setenv("SWAGGER_AGGREGATOR_PROBE_INTERVAL", "15") // the classic: a duration without a unit
	t.Setenv("SWAGGER_AGGREGATOR_FETCH_TIMEOUT", "fast")
	t.Setenv("SWAGGER_AGGREGATOR_MAX_SPEC_BYTES", "10MB")
	t.Setenv("SWAGGER_AGGREGATOR_KUBERNETES", "maybe")
	_, err := load(t)
	got := problems(t, err)
	want := map[string]string{
		"probe-interval": `"15" is not a duration`,
		"fetch-timeout":  `"fast" is not a duration`,
		"max-spec-bytes": `"10MB" is not an integer`,
		"kubernetes":     `"maybe" is not true or false`,
	}
	if len(got) != len(want) {
		t.Fatalf("want %d problems, got %v", len(want), got)
	}
	for _, p := range got {
		if w, ok := want[p.Field]; !ok || !strings.Contains(p.Message, w) {
			t.Errorf("unexpected problem %v", p)
		}
	}
}

func TestEverythingWrongIsReportedTogether(t *testing.T) {
	t.Parallel()

	_, err := load(
		t,
		"--listen",
		"nope",
		"--label-prefix",
		"bad prefix",
		"--probe-interval",
		"1ns",
		"--kubeconfig",
		"/x",
	)
	got := problems(t, err)
	fields := map[string]bool{}
	for _, p := range got {
		fields[p.Field] = true
	}
	for _, f := range []string{"listen", "label-prefix", "probe-interval", "kubeconfig"} {
		if !fields[f] {
			t.Errorf("--%s not reported in %v", f, got)
		}
	}
}

func TestSplitList(t *testing.T) {
	t.Parallel()

	got := splitList([]string{"a,b", " c ", "", ",d,,"})
	if strings.Join(got, "|") != "a|b|c|d" {
		t.Fatalf("got %v", got)
	}
}
