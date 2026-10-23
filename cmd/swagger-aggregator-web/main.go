// swagger-aggregator-web serves the built UI (web/dist) and forwards /api to the API server.
package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"
	"go.uber.org/zap"

	"github.com/perryprojects/docker-swagger-aggregator/internal/logging"
	"github.com/perryprojects/docker-swagger-aggregator/internal/server"
)

const (
	envPrefix         = "SWAGGER_AGGREGATOR_WEB"
	defaultListen     = ":3000"
	readHeaderTimeout = 10 * time.Second
	shutdownTimeout   = 5 * time.Second
)

func main() {
	if err := newCmd().Execute(); err != nil {
		os.Exit(1)
	}
}

func newCmd() *cobra.Command {
	v := viper.New()
	cmd := &cobra.Command{
		Use:          "swagger-aggregator-web",
		Short:        "Serve the built swagger-aggregator UI and forward /api to the API",
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := v.BindPFlags(cmd.Flags()); err != nil {
				return err
			}
			return run(cmd.Context(), v)
		},
	}
	f := cmd.Flags()
	f.String("listen", defaultListen, "HTTP listen address")
	f.String("dir", "web/dist", "directory with the built UI")
	f.String("api", "http://localhost:8080", "base URL of the swagger-aggregator API")
	f.String("log-level", "info", "debug, info, warn or error")
	f.String("log-format", logging.FormatConsole, "console (human readable) or json (for log collectors)")
	v.SetEnvPrefix(envPrefix)
	v.SetEnvKeyReplacer(strings.NewReplacer("-", "_"))
	v.AutomaticEnv()
	cmd.AddCommand(healthcheckCmd(v))
	return cmd
}

// healthcheckCmd probes this program's own /healthz (used as the container HEALTHCHECK).
func healthcheckCmd(v *viper.Viper) *cobra.Command {
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

	api, err := url.Parse(v.GetString("api"))
	if err != nil || api.Host == "" {
		return fmt.Errorf("invalid --api %q", v.GetString("api"))
	}
	dir := v.GetString("dir")
	if _, err = os.Stat(dir + "/index.html"); err != nil {
		return fmt.Errorf("no built UI in %q (run `bun run build` in web/): %w", dir, err)
	}

	ctx, stop := signal.NotifyContext(parent, os.Interrupt, syscall.SIGTERM)
	defer stop()

	// No WriteTimeout: it would kill the proxied SSE stream.
	srv := &http.Server{
		Addr:              v.GetString("listen"),
		Handler:           server.NewWebHandler(server.WebConfig{UI: os.DirFS(dir), API: api}),
		ReadHeaderTimeout: readHeaderTimeout,
	}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownTimeout)
		defer cancel()
		if shutdownErr := srv.Shutdown(shutdown); shutdownErr != nil {
			log.Error("shutdown failed", zap.Error(shutdownErr))
		}
	}()
	log.Info("serving", zap.String("dir", dir), zap.String("addr", srv.Addr), zap.String("api", api.String()))
	if err = srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
