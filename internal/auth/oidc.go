// Package auth protects the web server with OpenID Connect: the authorization code flow with PKCE
// (coreos/go-oidc + golang.org/x/oauth2) and a signed, encrypted session cookie (gorilla/securecookie).
// Any OIDC provider works (Authelia, Keycloak, Authentik, Entra ID, Google, ...).
package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/gorilla/securecookie"
	"go.uber.org/zap"
	"golang.org/x/oauth2"
)

const (
	errorKey           = "error" // the JSON key of every error response
	errUnauthenticated = "unauthenticated"
	schemeHTTP         = "http"
	schemeHTTPS        = "https"

	loginPath    = "/auth/login"
	callbackPath = "/auth/callback"
	logoutPath   = "/auth/logout"

	sessionCookie = "swagger_aggregator_session"
	flowCookie    = "swagger_aggregator_oidc"
	flowTTL       = 10 * time.Minute
	minSecretLen  = 32

	flowStateLen       = 24 // random bytes of the state and nonce values
	defaultTTL         = 8 * time.Hour
	defaultGroupsClaim = "groups"
)

// DefaultScopes asks for the groups claim too; providers that do not know the scope ignore it.
func DefaultScopes() []string {
	return []string{oidc.ScopeOpenID, "profile", "email", "groups"}
}

// Config configures the OIDC login. All fields except the optional ones are required.
type Config struct {
	Issuer       string
	ClientID     string
	ClientSecret string
	// RedirectURL is the public callback URL registered at the provider, e.g.
	// https://docs.example.com/auth/callback. Its path must be BasePath + "/auth/callback".
	RedirectURL string
	// SessionSecret signs and encrypts the cookies (at least 32 bytes of randomness).
	SessionSecret string
	// BasePath is the public path prefix the UI is served under ("" or "/docs").
	BasePath string

	// Optional.
	Scopes      []string      // default DefaultScopes
	GroupsClaim string        // default "groups"
	SessionTTL  time.Duration // default 8h
	// AllowedGroups / AllowedEmails restrict who may sign in: a user needs one of the groups or one of
	// the (verified) e-mail addresses. Both empty: every user the provider authenticates.
	AllowedGroups []string
	AllowedEmails []string
	// SameSite of the cookies: Lax (the default, also for the zero value) or None for a frontend on another *site*
	// (None implies Secure, so it needs HTTPS or localhost).
	SameSite http.SameSite
	// HTTPClient is used for the provider (discovery, keys, token, userinfo); default http.DefaultClient.
	HTTPClient *http.Client
}

// OIDC is the login handler and middleware. Create it with New.
type OIDC struct {
	cfg        Config
	log        *zap.Logger
	cookies    *securecookie.SecureCookie
	secure     bool   // Secure cookie attribute: the public URL is https
	cookiePath string // cookies are scoped to the base path

	mu       sync.Mutex
	provider *oidc.Provider // resolved lazily, so the IdP may start after this server
	oauth    *oauth2.Config
	verifier *oidc.IDTokenVerifier
}

type session struct {
	Sub   string `json:"sub"`
	Email string `json:"email,omitempty"`
	Name  string `json:"name,omitempty"`
}

type flow struct {
	State    string `json:"s"`
	Nonce    string `json:"n"`
	Verifier string `json:"v"`
	Return   string `json:"r"`
}

// New validates the configuration. It does not contact the provider.
func New(cfg Config, log *zap.Logger) (*OIDC, error) {
	if cfg.Scopes == nil {
		cfg.Scopes = DefaultScopes()
	}
	if cfg.GroupsClaim == "" {
		cfg.GroupsClaim = defaultGroupsClaim
	}
	if cfg.SessionTTL <= 0 {
		cfg.SessionTTL = defaultTTL
	}
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	// Two independent keys from one secret: HMAC (integrity) and AES-256 (confidentiality).
	hashKey := sha256.Sum256([]byte("hash\x00" + cfg.SessionSecret))
	blockKey := sha256.Sum256([]byte("block\x00" + cfg.SessionSecret))
	sc := securecookie.New(hashKey[:], blockKey[:]).SetSerializer(securecookie.JSONEncoder{})

	path := cfg.BasePath
	if path == "" {
		path = "/"
	}
	return &OIDC{
		cfg: cfg, log: log, cookies: sc, cookiePath: path,
		secure: strings.HasPrefix(cfg.RedirectURL, "https://") || cfg.SameSite == http.SameSiteNoneMode,
	}, nil
}

func (c Config) validate() error {
	var errs []error
	if u, err := url.Parse(
		c.Issuer,
	); err != nil || (u.Scheme != schemeHTTP && u.Scheme != schemeHTTPS) ||
		u.Host == "" {
		errs = append(errs, fmt.Errorf("oidc issuer %q must be an http(s) URL", c.Issuer))
	}
	if c.ClientID == "" {
		errs = append(errs, errors.New("oidc client id is required"))
	}
	if c.ClientSecret == "" {
		errs = append(errs, errors.New("oidc client secret is required"))
	}
	if len(c.SessionSecret) < minSecretLen {
		errs = append(
			errs,
			fmt.Errorf("session secret must be at least %d characters (e.g. `openssl rand -base64 48`)", minSecretLen),
		)
	}
	u, err := url.Parse(c.RedirectURL)
	switch {
	case err != nil || (u.Scheme != schemeHTTP && u.Scheme != schemeHTTPS) || u.Host == "":
		errs = append(errs, fmt.Errorf("oidc redirect url %q must be an absolute http(s) URL", c.RedirectURL))
	case u.Path != c.BasePath+callbackPath:
		errs = append(errs, fmt.Errorf("oidc redirect url path must be %q, got %q", c.BasePath+callbackPath, u.Path))
	}
	return errors.Join(errs...)
}

// Wrap protects next. The /auth/* routes and /healthz stay reachable; everything else needs a session.
// Browser navigations are sent to the provider, API calls (fetch, EventSource, the proxies) get 401.
func (a *OIDC) Wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case loginPath:
			a.login(w, r)
			return
		case callbackPath:
			a.callback(w, r)
			return
		case logoutPath:
			a.logout(w, r)
			return
		case "/healthz": // container/kubelet probes carry no session
			next.ServeHTTP(w, r)
			return
		}
		if _, ok := a.session(r); ok {
			w.Header().Set("Cache-Control", "private, no-store")
			next.ServeHTTP(w, r)
			return
		}
		if r.Method == http.MethodGet && strings.Contains(r.Header.Get("Accept"), "text/html") {
			ret := a.cfg.BasePath + r.URL.Path
			if r.URL.RawQuery != "" {
				ret += "?" + r.URL.RawQuery
			}
			http.Redirect(w, r, a.cfg.BasePath+loginPath+"?rd="+url.QueryEscape(ret), http.StatusFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusUnauthorized)
		_ = json.NewEncoder(w).
			Encode(map[string]string{errorKey: errUnauthenticated, "login": a.cfg.BasePath + loginPath})
	})
}

func (a *OIDC) session(r *http.Request) (session, bool) {
	c, err := r.Cookie(sessionCookie)
	if err != nil {
		return session{}, false
	}
	var s session
	if err = a.cookies.Decode(sessionCookie, c.Value, &s); err != nil || s.Sub == "" {
		return session{}, false
	}
	return s, true
}

// ensure resolves the provider (discovery document and keys) once; until then login answers 503.
func (a *OIDC) ensure(ctx context.Context) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.provider != nil {
		return nil
	}
	if a.cfg.HTTPClient != nil {
		ctx = oidc.ClientContext(ctx, a.cfg.HTTPClient)
	}
	p, err := oidc.NewProvider(ctx, a.cfg.Issuer)
	if err != nil {
		return err
	}
	a.provider = p
	a.verifier = p.Verifier(&oidc.Config{ClientID: a.cfg.ClientID})
	a.oauth = &oauth2.Config{
		ClientID: a.cfg.ClientID, ClientSecret: a.cfg.ClientSecret, RedirectURL: a.cfg.RedirectURL,
		Endpoint: p.Endpoint(), Scopes: a.cfg.Scopes,
	}
	return nil
}

func (a *OIDC) clientCtx(ctx context.Context) context.Context {
	if a.cfg.HTTPClient != nil {
		return oidc.ClientContext(ctx, a.cfg.HTTPClient)
	}
	return ctx
}

func (a *OIDC) login(w http.ResponseWriter, r *http.Request) {
	if err := a.ensure(r.Context()); err != nil {
		a.log.Warn("oidc provider unreachable", zap.Error(err))
		http.Error(w, "identity provider unavailable, try again shortly", http.StatusServiceUnavailable)
		return
	}
	f := flow{
		State: random(), Nonce: random(), Verifier: oauth2.GenerateVerifier(),
		Return: a.safeReturn(r.URL.Query().Get("rd")),
	}
	if !a.setCookie(w, flowCookie, f, flowTTL) {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, a.oauth.AuthCodeURL(f.State, oidc.Nonce(f.Nonce), oauth2.S256ChallengeOption(f.Verifier)),
		http.StatusFound)
}

func (a *OIDC) callback(w http.ResponseWriter, r *http.Request) {
	var f flow
	c, err := r.Cookie(flowCookie)
	if err != nil || a.cookies.Decode(flowCookie, c.Value, &f) != nil {
		http.Error(w, "login session expired, start again", http.StatusBadRequest)
		return
	}
	a.clearCookie(w, flowCookie)
	q := r.URL.Query()
	if e := q.Get(errorKey); e != "" {
		a.log.Info("oidc login refused by the provider", zap.String("error", e))
		http.Error(w, "login refused by the identity provider", http.StatusForbidden)
		return
	}
	if subtle.ConstantTimeCompare([]byte(q.Get("state")), []byte(f.State)) != 1 || q.Get("code") == "" {
		http.Error(w, "invalid login response", http.StatusBadRequest)
		return
	}
	if err = a.ensure(r.Context()); err != nil {
		http.Error(w, "identity provider unavailable, try again shortly", http.StatusServiceUnavailable)
		return
	}

	ctx := a.clientCtx(r.Context())
	idToken, claims, err := a.exchange(ctx, q.Get("code"), f)
	if err != nil {
		http.Error(w, "login failed", http.StatusBadGateway)
		return
	}

	s := session{
		Sub:   idToken.Subject,
		Email: str(claims["email"]),
		Name:  first(str(claims["name"]), str(claims["preferred_username"])),
	}
	if !a.allowed(claims) {
		a.log.Info("oidc user not allowed", zap.String("sub", s.Sub), zap.String("email", s.Email))
		http.Error(w, "you are not allowed to use this application", http.StatusForbidden)
		return
	}
	if !a.setCookie(w, sessionCookie, s, a.cfg.SessionTTL) {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, f.Return, http.StatusFound)
}

// exchange trades the code for tokens, verifies the ID token and returns it with its claims (plus those of the
// userinfo endpoint when the access rules need claims the ID token lacks). It logs why it failed.
func (a *OIDC) exchange(ctx context.Context, code string, f flow) (*oidc.IDToken, map[string]any, error) {
	tok, err := a.oauth.Exchange(ctx, code, oauth2.VerifierOption(f.Verifier))
	if err != nil {
		a.log.Warn("oidc code exchange failed", zap.Error(err))
		return nil, nil, err
	}
	raw, _ := tok.Extra("id_token").(string)
	idToken, err := a.verifier.Verify(ctx, raw)
	if err == nil && subtle.ConstantTimeCompare([]byte(idToken.Nonce), []byte(f.Nonce)) != 1 {
		err = errors.New("nonce mismatch")
	}
	if err != nil {
		a.log.Warn("oidc id token rejected", zap.Error(err))
		return nil, nil, err
	}
	claims := map[string]any{}
	if err = idToken.Claims(&claims); err != nil {
		return nil, nil, err
	}
	if a.restricted() && !a.hasAuthzClaims(claims) {
		a.addUserInfoClaims(ctx, tok, idToken, claims)
	}
	return idToken, claims, nil
}

// addUserInfoClaims adds the userinfo claims that the ID token lacks. Some providers (e.g. Authelia) only put
// groups into userinfo.
func (a *OIDC) addUserInfoClaims(ctx context.Context, tok *oauth2.Token, idToken *oidc.IDToken, claims map[string]any) {
	ui, err := a.provider.UserInfo(ctx, oauth2.StaticTokenSource(tok))
	if err != nil {
		return
	}
	extra := map[string]any{}
	if ui.Claims(&extra) != nil || ui.Subject != idToken.Subject {
		return
	}
	for k, v := range extra {
		if _, has := claims[k]; !has {
			claims[k] = v
		}
	}
}

func (a *OIDC) logout(w http.ResponseWriter, r *http.Request) {
	a.clearCookie(w, sessionCookie)
	http.Redirect(w, r, a.cfg.BasePath+"/", http.StatusFound)
}

func (a *OIDC) restricted() bool { return len(a.cfg.AllowedGroups) > 0 || len(a.cfg.AllowedEmails) > 0 }

// hasAuthzClaims reports whether the claims carry what the restrictions are checked against.
func (a *OIDC) hasAuthzClaims(claims map[string]any) bool {
	if len(a.cfg.AllowedGroups) > 0 {
		if _, ok := claims[a.cfg.GroupsClaim]; ok {
			return true
		}
	}
	if len(a.cfg.AllowedEmails) > 0 {
		if _, ok := claims["email"]; ok {
			return true
		}
	}
	return false
}

// allowed applies AllowedGroups / AllowedEmails. An unverified e-mail address proves nothing.
func (a *OIDC) allowed(claims map[string]any) bool {
	if !a.restricted() {
		return true
	}
	for _, g := range stringList(claims[a.cfg.GroupsClaim]) {
		if slices.Contains(a.cfg.AllowedGroups, g) {
			return true
		}
	}
	if verified, ok := claims["email_verified"].(bool); ok && !verified {
		return false
	}
	email := str(claims["email"])
	return email != "" &&
		slices.ContainsFunc(a.cfg.AllowedEmails, func(e string) bool { return strings.EqualFold(e, email) })
}

// safeReturn accepts only a local path (with the base path), so the login cannot be turned into an open redirect.
func (a *OIDC) safeReturn(rd string) string {
	fallback := a.cfg.BasePath + "/"
	if rd == "" || !strings.HasPrefix(rd, "/") || strings.HasPrefix(rd, "//") || strings.ContainsAny(rd, "\\\r\n") {
		return fallback
	}
	if u, err := url.Parse(rd); err != nil || u.Host != "" || u.Scheme != "" {
		return fallback
	}
	if a.cfg.BasePath != "" && rd != a.cfg.BasePath && !strings.HasPrefix(rd, a.cfg.BasePath+"/") {
		return fallback
	}
	return rd
}

func (a *OIDC) setCookie(w http.ResponseWriter, name string, v any, ttl time.Duration) bool {
	sc := *a.cookies // copy: MaxAge is per cookie kind
	sc.MaxAge(int(ttl.Seconds()))
	enc, err := sc.Encode(name, v)
	if err != nil {
		a.log.Error("cannot encode cookie", zap.String("cookie", name), zap.Error(err))
		return false
	}
	//nolint:gosec // Secure is decided once per deployment (a.secure), plain-HTTP setups still work
	http.SetCookie(w, &http.Cookie{
		Name: name, Value: enc, Path: a.cookiePath, MaxAge: int(ttl.Seconds()),
		HttpOnly: true, Secure: a.secure, SameSite: sameSiteOrLax(a.cfg.SameSite),
	})
	return true
}

func (a *OIDC) clearCookie(w http.ResponseWriter, name string) {
	//nolint:gosec // see setCookie
	http.SetCookie(w, &http.Cookie{
		Name: name, Value: "", Path: a.cookiePath, MaxAge: -1,
		HttpOnly: true, Secure: a.secure, SameSite: sameSiteOrLax(a.cfg.SameSite),
	})
}

func random() string {
	b := make([]byte, flowStateLen)
	if _, err := rand.Read(b); err != nil {
		panic(err) // crypto/rand does not fail on supported platforms; never continue with a guessable state
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

func str(v any) string { s, _ := v.(string); return s }

func first(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// stringList reads a claim that is a string or a list of strings.
func stringList(v any) []string {
	switch t := v.(type) {
	case string:
		return []string{t}
	case []any:
		out := make([]string, 0, len(t))
		for _, e := range t {
			if s, ok := e.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

// sameSiteOrLax maps the zero value of a config field to the default, Lax.
func sameSiteOrLax(s http.SameSite) http.SameSite {
	if s == 0 {
		return http.SameSiteLaxMode
	}
	return s
}
