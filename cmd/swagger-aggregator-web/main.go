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

	"github.com/perryprojects/docker-swagger-aggregator/internal/auth"
	"github.com/perryprojects/docker-swagger-aggregator/internal/logging"
	"github.com/perryprojects/docker-swagger-aggregator/internal/server"
)

const (
	envPrefix         = "SWAGGER_AGGREGATOR_WEB"
	defaultListen     = ":3000"
	authNone          = "none"
	authOIDC          = "oidc"
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
	f.String("base-path", "", "public path prefix the UI is served under, e.g. /docs (default: served at /)")
	f.String("auth", authNone, "authentication: none or oidc (login with any OpenID Connect provider, e.g. Authelia)")
	f.String("oidc-issuer", "", "OIDC issuer URL (auth=oidc), e.g. https://auth.example.com")
	f.String("oidc-client-id", "", "OIDC client id (auth=oidc)")
	f.String("oidc-client-secret", "", "OIDC client secret (auth=oidc; prefer the environment variable)")
	f.String("oidc-redirect-url", "", "public callback URL registered at the provider: <public url>[/base-path]/auth/callback")
	f.StringSlice("oidc-scopes", auth.DefaultScopes, "OIDC scopes to request")
	f.String("oidc-groups-claim", "groups", "claim that lists the user's groups")
	f.StringSlice("oidc-allowed-groups", nil, "only users in one of these groups may sign in (default: every authenticated user)")
	f.StringSlice("oidc-allowed-emails", nil, "only these (verified) e-mail addresses may sign in (alternative to groups)")
	f.String("session-secret", "", "secret that signs and encrypts the session cookie, >= 32 characters (auth=oidc; use the environment variable)")
	f.Duration("session-ttl", 8*time.Hour, "how long a login lasts (auth=oidc)")
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

	base := strings.TrimRight(v.GetString("base-path"), "/")
	if base != "" && (!strings.HasPrefix(base, "/") || strings.ContainsAny(base, "?# ")) {
		return fmt.Errorf("invalid --base-path %q (want e.g. /docs)", v.GetString("base-path"))
	}

	protect, err := authMiddleware(v, base, log)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(parent, os.Interrupt, syscall.SIGTERM)
	defer stop()

	// No WriteTimeout: it would kill the proxied SSE stream.
	srv := &http.Server{
		Addr:              v.GetString("listen"),
		Handler:           server.NewWebHandler(server.WebConfig{UI: os.DirFS(dir), API: api, BasePath: base, Protect: protect}),
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

// authMiddleware builds the login protection selected by --auth; nil means unprotected.
func authMiddleware(v *viper.Viper, base string, log *zap.Logger) (func(http.Handler) http.Handler, error) {
	switch mode := v.GetString("auth"); mode {
	case "", authNone:
		if v.GetString("oidc-issuer") != "" || v.GetString("oidc-client-id") != "" {
			log.Warn("oidc settings are ignored because --auth is none: the UI and API are NOT protected")
		}
		return nil, nil
	case authOIDC:
		o, err := auth.New(auth.Config{
			Issuer:        v.GetString("oidc-issuer"),
			ClientID:      v.GetString("oidc-client-id"),
			ClientSecret:  v.GetString("oidc-client-secret"),
			RedirectURL:   v.GetString("oidc-redirect-url"),
			SessionSecret: v.GetString("session-secret"),
			BasePath:      base,
			Scopes:        splitList(v.GetStringSlice("oidc-scopes")),
			GroupsClaim:   v.GetString("oidc-groups-claim"),
			SessionTTL:    v.GetDuration("session-ttl"),
			AllowedGroups: splitList(v.GetStringSlice("oidc-allowed-groups")),
			AllowedEmails: splitList(v.GetStringSlice("oidc-allowed-emails")),
		}, log)
		if err != nil {
			return nil, fmt.Errorf("--auth=oidc: %w", err)
		}
		log.Info("authentication enabled", zap.String("mode", mode), zap.String("issuer", v.GetString("oidc-issuer")))
		return o.Wrap, nil
	default:
		return nil, fmt.Errorf("invalid --auth %q (none or oidc)", mode)
	}
}

// splitList accepts repeated flags and comma separated values.
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
