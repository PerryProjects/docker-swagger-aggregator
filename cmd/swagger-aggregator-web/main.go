// swagger-aggregator-web serves the built UI (web/dist) and forwards /api to the API server.
package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"
	"go.uber.org/zap"

	"github.com/perryprojects/docker-swagger-aggregator/internal/auth"
	"github.com/perryprojects/docker-swagger-aggregator/internal/logging"
	"github.com/perryprojects/docker-swagger-aggregator/internal/mcptools"
	"github.com/perryprojects/docker-swagger-aggregator/internal/server"
	"github.com/perryprojects/docker-swagger-aggregator/internal/store"
)

const (
	envPrefix         = "SWAGGER_AGGREGATOR_WEB"
	defaultListen     = ":3000"
	defaultSessionTTL = 8 * time.Hour
	secretBytes       = 48 // random bytes of a generated secret
	authNone          = "none"
	authOIDC          = "oidc"
	authBuiltIn       = "build_in"
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
	f.String(
		"auth",
		authNone,
		"authentication: none, build_in (own login form + users in SQLite) or oidc (e.g. Authelia)",
	)
	f.String("data-dir", "data", "directory for the user database and the session key (auth=build_in)")
	f.String("admin-user", "", "build_in: create this admin on first start (instead of the setup page)")
	f.String(
		"admin-password",
		"",
		"build_in: password of --admin-user (>= 10 characters; use the environment variable)",
	)
	f.String("admin-password-hash", "", "build_in: bcrypt hash of the admin password, instead of --admin-password")
	f.String(
		"setup-token",
		"",
		"build_in: token for registering the first admin on the setup page (default: random, printed in the log)",
	)
	f.String("oidc-issuer", "", "OIDC issuer URL (auth=oidc), e.g. https://auth.example.com")
	f.String("oidc-client-id", "", "OIDC client id (auth=oidc)")
	f.String("oidc-client-secret", "", "OIDC client secret (auth=oidc; prefer the environment variable)")
	f.String(
		"oidc-redirect-url",
		"",
		"public callback URL registered at the provider: <public url>[/base-path]/auth/callback",
	)
	f.StringSlice("oidc-scopes", auth.DefaultScopes(), "OIDC scopes to request")
	f.String("oidc-groups-claim", "groups", "claim that lists the user's groups")
	f.StringSlice(
		"oidc-allowed-groups",
		nil,
		"only users in one of these groups may sign in (default: every authenticated user)",
	)
	f.StringSlice(
		"oidc-allowed-emails",
		nil,
		"only these (verified) e-mail addresses may sign in (alternative to groups)",
	)
	f.String(
		"session-secret",
		"",
		"secret that signs and encrypts the session cookie, >= 32 characters (oidc: required; build_in: default is a key file in --data-dir)",
	)
	f.Duration("session-ttl", defaultSessionTTL, "how long a login lasts (auth=oidc)")
	f.Bool("mcp", false, "serve a read-only MCP server for AI assistants at /mcp (needs --mcp-token)")
	f.String("mcp-token", "", "bearer token the MCP clients must send, >= 32 characters (use the environment variable)")
	f.StringSlice(
		"cors-origins",
		nil,
		"origins (scheme://host[:port]) of a separate frontend that may use the API with the session cookie",
	)
	f.String(
		"cookie-samesite",
		"lax",
		"lax (default) or none: none lets a frontend on another site use the cookie (needs --cors-origins and HTTPS)",
	)
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

	corsOrigins, err := server.ParseCORSOrigins(splitList(v.GetStringSlice("cors-origins")))
	if err != nil {
		return fmt.Errorf("--cors-origins: %w", err)
	}
	sameSite, err := parseSameSite(v.GetString("cookie-samesite"), len(corsOrigins) > 0)
	if err != nil {
		return err
	}

	protect, closeAuth, err := authMiddleware(parent, v, base, sameSite, log)
	if err != nil {
		return err
	}
	defer closeAuth()

	var mcpHandler http.Handler
	if v.GetBool("mcp") {
		if mcpHandler, err = mcptools.Handler(
			mcptools.HTTPBackend{Base: api},
			version(),
			v.GetString("mcp-token"),
		); err != nil {
			return fmt.Errorf("--mcp: %w", err)
		}
		log.Info("MCP server enabled", zap.String("path", base+"/mcp"))
	} else if v.GetString("mcp-token") != "" {
		log.Warn("--mcp-token is ignored because --mcp is off")
	}

	ctx, stop := signal.NotifyContext(parent, os.Interrupt, syscall.SIGTERM)
	defer stop()

	// No WriteTimeout: it would kill the proxied SSE stream.
	srv := &http.Server{
		Addr: v.GetString("listen"),
		Handler: server.NewWebHandler(
			server.WebConfig{
				UI:          os.DirFS(dir),
				API:         api,
				BasePath:    base,
				Protect:     protect,
				MCP:         mcpHandler,
				CORSOrigins: corsOrigins,
			},
		),
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

// authMiddleware builds the login protection selected by --auth; nil means unprotected. The returned func
// releases what it opened (the database).
func authMiddleware(
	ctx context.Context,
	v *viper.Viper,
	base string,
	sameSite http.SameSite,
	log *zap.Logger,
) (func(http.Handler) http.Handler, func(), error) {
	noop := func() {}
	switch mode := v.GetString("auth"); mode {
	case "", authNone:
		if v.GetString("oidc-issuer") != "" || v.GetString("oidc-client-id") != "" || v.GetString("admin-user") != "" {
			log.Warn("auth settings are ignored because --auth is none: the UI and API are NOT protected")
		}
		return nil, noop, nil
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
			SameSite:      sameSite,
			AllowedGroups: splitList(v.GetStringSlice("oidc-allowed-groups")),
			AllowedEmails: splitList(v.GetStringSlice("oidc-allowed-emails")),
		}, log)
		if err != nil {
			return nil, noop, fmt.Errorf("--auth=oidc: %w", err)
		}
		log.Info("authentication enabled", zap.String("mode", mode), zap.String("issuer", v.GetString("oidc-issuer")))
		return o.Wrap, noop, nil
	case authBuiltIn:
		return builtInAuth(ctx, v, base, sameSite, log)
	default:
		return nil, noop, fmt.Errorf("invalid --auth %q (none, build_in or oidc)", mode)
	}
}

func builtInAuth(
	ctx context.Context,
	v *viper.Viper,
	base string,
	sameSite http.SameSite,
	log *zap.Logger,
) (func(http.Handler) http.Handler, func(), error) {
	noop := func() {}
	dir := v.GetString("data-dir")
	db, err := store.Open(ctx, dir)
	if err != nil {
		return nil, noop, fmt.Errorf("--auth=build_in: %w (is %q writable?)", err, dir)
	}
	closer := func() { _ = db.Close() }
	secret := v.GetString("session-secret")
	if secret == "" {
		if secret, err = sessionKey(dir); err != nil {
			closer()
			return nil, noop, fmt.Errorf("--auth=build_in: session key: %w", err)
		}
	}
	l, err := auth.NewLocal(auth.LocalConfig{
		Store:         db,
		SessionSecret: secret,
		BasePath:      base,
		SessionTTL:    v.GetDuration("session-ttl"),
		SameSite:      sameSite,
		SetupToken:    v.GetString("setup-token"),
	}, log)
	if err != nil {
		closer()
		return nil, noop, fmt.Errorf("--auth=build_in: %w", err)
	}
	if err = l.Bootstrap(
		ctx,
		v.GetString("admin-user"),
		v.GetString("admin-password"),
		v.GetString("admin-password-hash"),
	); err != nil {
		closer()
		return nil, noop, fmt.Errorf("--auth=build_in: %w", err)
	}
	log.Info("authentication enabled", zap.String("mode", authBuiltIn), zap.String("dataDir", dir))
	return l.Wrap, closer, nil
}

// sessionKey returns the cookie secret kept in <dir>/session.key, creating it on first use, so that logins
// survive a restart without anybody having to invent a secret.
func sessionKey(dir string) (string, error) {
	path := filepath.Join(dir, "session.key")
	if b, err := os.ReadFile(path); err == nil && len(strings.TrimSpace(string(b))) >= 32 {
		return strings.TrimSpace(string(b)), nil
	}
	raw := make([]byte, secretBytes)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	key := base64.RawURLEncoding.EncodeToString(raw)
	return key, os.WriteFile(path, []byte(key+"\n"), 0o600)
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

// version is the module version of the binary ("devel" for a plain `go build`), reported to MCP clients.
func version() string {
	if bi, ok := debug.ReadBuildInfo(); ok && bi.Main.Version != "" {
		return bi.Main.Version
	}
	return "dev"
}

// parseSameSite reads --cookie-samesite. "none" only makes sense (and is only safe) together with a CORS list: the
// browser no longer keeps other sites from sending the cookie, so the server's origin check has to be on.
func parseSameSite(value string, corsOn bool) (http.SameSite, error) {
	switch strings.ToLower(value) {
	case "", "lax":
		return http.SameSiteLaxMode, nil
	case "none":
		if !corsOn {
			return 0, errors.New(
				"--cookie-samesite=none needs --cors-origins: it is for a frontend on another site, and the CORS list is what protects the cookie then",
			)
		}
		return http.SameSiteNoneMode, nil
	}
	return 0, fmt.Errorf("invalid --cookie-samesite %q (lax or none)", value)
}
