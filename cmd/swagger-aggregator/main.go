// swagger-aggregator discovers OpenAPI/Swagger specs of labeled workloads (Docker containers, Kubernetes
// Services) and serves them (API only; the UI is served by swagger-aggregator-web).
package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"github.com/spf13/viper"

	"github.com/perryprojects/docker-swagger-aggregator/internal/app"
	"github.com/perryprojects/docker-swagger-aggregator/internal/logging"
	"github.com/perryprojects/docker-swagger-aggregator/internal/server"
)

const envPrefix = "SWAGGER_AGGREGATOR"

func main() {
	if err := newCmd().Execute(); err != nil {
		os.Exit(1)
	}
}

func newCmd() *cobra.Command {
	v := newViper()
	cmd := &cobra.Command{
		Use:          "swagger-aggregator",
		Short:        "Aggregate OpenAPI specs of labeled Docker containers and Kubernetes Services into one Scalar UI",
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := v.BindPFlags(cmd.Flags()); err != nil {
				return err
			}
			return run(cmd.Context(), v)
		},
	}
	registerFlags(cmd.Flags(), app.DefaultConfig())
	cmd.AddCommand(healthcheckCmd(v, app.DefaultConfig().Listen))
	return cmd
}

// newViper reads SWAGGER_AGGREGATOR_<FLAG> environment variables ('-' becomes '_').
func newViper() *viper.Viper {
	v := viper.New()
	v.SetEnvPrefix(envPrefix)
	v.SetEnvKeyReplacer(strings.NewReplacer("-", "_"))
	v.AutomaticEnv()
	return v
}

func registerFlags(f *pflag.FlagSet, def app.Config) {
	f.String("listen", def.Listen, "HTTP listen address")
	f.String("docker-host", def.DockerHost,
		"Docker daemon address, e.g. tcp://socket-proxy:2375 (default: DOCKER_HOST, else the local socket)")
	f.String("network", def.Network, "Docker network used to reach containers (default: first network with an IP)")
	f.Duration("probe-interval", def.ProbeInterval,
		"how often each spec is re-fetched (liveness probe); also the retry backoff cap")
	f.Duration("fetch-timeout", def.FetchTimeout, "per-request timeout when fetching a spec")
	f.Int64("max-spec-bytes", def.MaxSpecBytes, "maximum accepted spec size in bytes")
	f.Duration("shutdown-grace", def.ShutdownGrace, "how long in-flight requests get to finish on shutdown")
	f.Bool("derive-server-url", def.DeriveServerURL,
		"derive a missing server-url from Traefik router labels (Docker) or Ingress/IngressRoute objects (Kubernetes)")
	f.String("label-prefix", def.LabelPrefix,
		"label namespace to watch, e.g. to run staging and production side by side")
	f.Bool("prefer-published-ports", def.PreferPublishedPorts,
		"reach containers via their published host port (127.0.0.1) instead of the network IP; "+
			"for running on the host with Docker Desktop")
	f.String("base-path", def.BasePath, "public path prefix the UI is served under, e.g. /docs (default: /)")
	f.Bool("insecure-upstream-tls", def.InsecureUpstreamTLS,
		"skip certificate verification for workloads labeled scheme=https (they are reached by IP)")
	f.Bool("docker", def.DockerEnabled,
		"watch Docker containers (disable when running inside Kubernetes without a Docker socket)")
	f.Bool("kubernetes", def.Kubernetes.Enabled, "watch Kubernetes Services")
	f.String("kubeconfig", def.Kubernetes.Kubeconfig,
		"kubeconfig path (default: in-cluster ServiceAccount if available, else KUBECONFIG / ~/.kube/config)")
	f.String("kube-context", def.Kubernetes.Context, "kubeconfig context (default: the current one)")
	f.StringSlice("namespace", def.Kubernetes.Namespaces,
		"Kubernetes namespace to watch, repeatable or comma separated (default: all, needs cluster-wide RBAC)")
	f.String(
		"kube-access",
		def.Kubernetes.Access,
		"how Services are reached: service (in-cluster DNS), apiserver (API server service proxy, works from outside), auto",
	)
	f.String("cluster-domain", def.Kubernetes.ClusterDomain, "cluster DNS suffix for --kube-access=service")
	f.String("log-level", "info", "debug, info, warn or error")
	f.String("log-format", logging.FormatConsole, "console (human readable) or json (for log collectors)")
}

// settings parses typed values strictly. A flag is already typed by pflag, but the same setting can come
// from the environment as free text, and viper would turn "abc" into 0 silently.
type settings struct {
	v        *viper.Viper
	problems []app.FieldError
}

func (s *settings) failf(key, format string, args ...any) {
	s.problems = append(s.problems, app.FieldError{Field: key, Message: fmt.Sprintf(format, args...)})
}

func (s *settings) duration(key string) time.Duration {
	raw := s.v.GetString(key)
	d, err := time.ParseDuration(raw)
	if err != nil {
		s.failf(key, "%q is not a duration (use a unit, e.g. 500ms, 15s, 2m)", raw)
	}
	return d
}

func (s *settings) integer(key string) int64 {
	raw := s.v.GetString(key)
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		s.failf(key, "%q is not an integer", raw)
	}
	return n
}

func (s *settings) boolean(key string) bool {
	raw := s.v.GetString(key)
	b, err := strconv.ParseBool(raw)
	if err != nil {
		s.failf(key, "%q is not true or false", raw)
	}
	return b
}

// loadConfig reads every setting, reports unparsable values, then validates the result. All problems
// are returned together as an *app.ValidationError.
func loadConfig(v *viper.Viper) (app.Config, error) {
	s := &settings{v: v}
	cfg := app.Config{
		Listen:               v.GetString("listen"),
		DockerHost:           v.GetString("docker-host"),
		Network:              v.GetString("network"),
		LabelPrefix:          v.GetString("label-prefix"),
		ProbeInterval:        s.duration("probe-interval"),
		FetchTimeout:         s.duration("fetch-timeout"),
		MaxSpecBytes:         s.integer("max-spec-bytes"),
		ShutdownGrace:        s.duration("shutdown-grace"),
		DeriveServerURL:      s.boolean("derive-server-url"),
		PreferPublishedPorts: s.boolean("prefer-published-ports"),
		InsecureUpstreamTLS:  s.boolean("insecure-upstream-tls"),
		BasePath:             strings.TrimRight(v.GetString("base-path"), "/"),
		DockerEnabled:        s.boolean("docker"),
		Kubernetes: app.KubernetesConfig{
			Enabled:       s.boolean("kubernetes"),
			Kubeconfig:    v.GetString("kubeconfig"),
			Context:       v.GetString("kube-context"),
			Namespaces:    splitList(v.GetStringSlice("namespace")),
			Access:        v.GetString("kube-access"),
			ClusterDomain: v.GetString("cluster-domain"),
		},
	}
	if len(s.problems) > 0 {
		return cfg, &app.ValidationError{Problems: s.problems} // zero values would only add noise to Validate
	}
	return cfg, cfg.Validate()
}

// healthcheckCmd probes this program's own /healthz (used as the container HEALTHCHECK).
func healthcheckCmd(v *viper.Viper, defaultListen string) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "healthcheck",
		Short: "Exit 0 if the local server answers /healthz",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := v.BindPFlags(cmd.Flags()); err != nil {
				return err
			}
			return server.Probe(cmd.Context(), http.DefaultClient, v.GetString("listen"))
		},
	}
	cmd.Flags().String("listen", defaultListen, "address the server listens on")
	return cmd
}

func run(parent context.Context, v *viper.Viper) error {
	log, err := logging.New(v.GetString("log-level"), v.GetString("log-format"), os.Stderr)
	if err != nil {
		return err
	}
	defer func() { _ = log.Sync() }()
	logging.RedirectKlog(log) // client-go's own messages go through the same logger

	cfg, err := loadConfig(v)
	if err != nil {
		return err
	}
	a, err := app.New(cfg, app.Deps{Log: log})
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(parent, os.Interrupt, syscall.SIGTERM)
	defer stop()
	return a.Run(ctx)
}

// splitList accepts both repeated flags and comma separated values (also from the environment).
func splitList(in []string) []string {
	var out []string
	for _, item := range in {
		for part := range strings.SplitSeq(item, ",") {
			if part = strings.TrimSpace(part); part != "" {
				out = append(out, part)
			}
		}
	}
	return out
}
