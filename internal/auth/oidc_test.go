package auth_test

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
	"go.uber.org/zap"

	"github.com/perryprojects/docker-swagger-aggregator/internal/auth"
)

const secret = "0123456789abcdef0123456789abcdef-secret"

// idp is a minimal OIDC provider: discovery, keys and a token endpoint that checks PKCE and signs an ID token.
type idp struct {
	*httptest.Server
	key *rsa.PrivateKey

	mu     sync.Mutex
	codes  map[string]issued // authorization code -> what the "user" authorized
	claims map[string]any    // extra ID token claims
	// userinfo, if set, is served at /userinfo (and keeps claims out of the ID token)
	userinfo map[string]any
}

type issued struct{ nonce, challenge string }

func newIDP(t *testing.T) *idp {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	p := &idp{key: key, codes: map[string]issued{}, claims: map[string]any{}}
	mux := http.NewServeMux()
	p.Server = httptest.NewServer(mux)
	t.Cleanup(p.Close)

	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer": p.URL, "authorization_endpoint": p.URL + "/authorize", "token_endpoint": p.URL + "/token",
			"jwks_uri": p.URL + "/keys", "userinfo_endpoint": p.URL + "/userinfo",
			"id_token_signing_alg_values_supported": []string{"RS256"},
		})
	})
	mux.HandleFunc("/keys", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{
			{Key: &key.PublicKey, KeyID: "k1", Algorithm: "RS256", Use: "sig"},
		}})
	})
	mux.HandleFunc("/token", p.token)
	mux.HandleFunc("/userinfo", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer at-1" {
			http.Error(w, "no", http.StatusUnauthorized)
			return
		}
		p.mu.Lock()
		defer p.mu.Unlock()
		_ = json.NewEncoder(w).Encode(p.userinfo)
	})
	return p
}

func (p *idp) token(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	p.mu.Lock()
	iss, ok := p.codes[r.Form.Get("code")]
	delete(p.codes, r.Form.Get("code")) // single use
	extra := map[string]any{}
	for k, v := range p.claims {
		extra[k] = v
	}
	p.mu.Unlock()
	sum := sha256.Sum256([]byte(r.Form.Get("code_verifier")))
	if !ok || base64.RawURLEncoding.EncodeToString(sum[:]) != iss.challenge {
		http.Error(w, `{"error":"invalid_grant"}`, http.StatusBadRequest) // PKCE or code mismatch
		return
	}
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: p.key},
		(&jose.SignerOptions{}).WithType("JWT").WithHeader("kid", "k1"))
	if err != nil {
		panic(err)
	}
	now := time.Now()
	claims := map[string]any{
		"iss": p.URL, "sub": "user-1", "aud": "client-1", "iat": now.Unix(), "exp": now.Add(time.Hour).Unix(),
		"nonce": iss.nonce,
	}
	for k, v := range extra {
		claims[k] = v
	}
	raw, err := jwt.Signed(signer).Claims(claims).Serialize()
	if err != nil {
		panic(err)
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "at-1", "token_type": "Bearer", "id_token": raw})
}

// authorize plays the user at the provider: it accepts the authorization request that login redirected to
// and returns the callback URL the provider would send the browser to.
func (p *idp) authorize(t *testing.T, authURL string) string {
	t.Helper()
	u, err := url.Parse(authURL)
	if err != nil || !strings.HasPrefix(authURL, p.URL+"/authorize") {
		t.Fatalf("login must redirect to the provider, got %q", authURL)
	}
	q := u.Query()
	if q.Get("code_challenge_method") != "S256" || q.Get("code_challenge") == "" || q.Get("nonce") == "" || q.Get("state") == "" {
		t.Fatalf("authorization request lacks PKCE/nonce/state: %v", q)
	}
	code := "code-" + q.Get("state")
	p.mu.Lock()
	p.codes[code] = issued{nonce: q.Get("nonce"), challenge: q.Get("code_challenge")}
	p.mu.Unlock()
	cb, _ := url.Parse(q.Get("redirect_uri"))
	cb.RawQuery = url.Values{"code": {code}, "state": {q.Get("state")}}.Encode()
	return cb.String()
}

type app struct {
	*httptest.Server
	idp *idp
}

// newApp serves a protected handler behind auth at a random port. base is the public base path.
func newApp(t *testing.T, p *idp, mutate func(*auth.Config)) *app {
	t.Helper()
	a := &app{idp: p}
	protected := http.NewServeMux()
	protected.HandleFunc("/api/services", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("[]")) })
	protected.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) })
	protected.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("INDEX")) })

	var handler http.Handler
	a.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { handler.ServeHTTP(w, r) }))
	t.Cleanup(a.Close)

	cfg := auth.Config{
		Issuer: p.URL, ClientID: "client-1", ClientSecret: "s3cret", SessionSecret: secret,
		RedirectURL: a.URL + "/auth/callback",
	}
	if mutate != nil {
		mutate(&cfg)
	}
	o, err := auth.New(cfg, zap.NewNop())
	if err != nil {
		t.Fatal(err)
	}
	handler = o.Wrap(protected)
	if base := cfg.BasePath; base != "" { // like the web server: the public prefix is stripped before auth runs
		inner := handler
		handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if rest, ok := strings.CutPrefix(r.URL.Path, base); ok {
				r = r.Clone(r.Context())
				r.URL.Path = "/" + strings.TrimPrefix(rest, "/")
			}
			inner.ServeHTTP(w, r)
		})
	}
	return a
}

// browser is a client with a cookie jar that does not follow redirects by itself.
func browser(t *testing.T) *http.Client {
	t.Helper()
	jar, _ := cookiejar.New(nil)
	return &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

func get(t *testing.T, c *http.Client, u string, accept string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, u, nil)
	if accept != "" {
		req.Header.Set("Accept", accept)
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

// signIn walks the whole flow and returns the response of the final redirect target.
func signIn(t *testing.T, a *app, c *http.Client, path string) *http.Response {
	t.Helper()
	r := get(t, c, a.URL+path, "text/html")
	if r.StatusCode != http.StatusFound || !strings.Contains(r.Header.Get("Location"), "/auth/login") {
		t.Fatalf("unauthenticated navigation must go to login: %d %q", r.StatusCode, r.Header.Get("Location"))
	}
	r = get(t, c, a.URL+r.Header.Get("Location"), "text/html") // login -> provider
	cb := a.idp.authorize(t, r.Header.Get("Location"))
	return get(t, c, cb, "text/html") // provider -> callback
}

func TestLoginFlowGrantsAccessAndKeepsTheRequestedPage(t *testing.T) {
	t.Parallel()

	a := newApp(t, newIDP(t), nil)
	c := browser(t)

	r := signIn(t, a, c, "/some/page?x=1")
	if r.StatusCode != http.StatusFound || r.Header.Get("Location") != "/some/page?x=1" {
		t.Fatalf("callback must return to the requested page: %d %q", r.StatusCode, r.Header.Get("Location"))
	}
	for _, ck := range r.Cookies() {
		if ck.Name == "swagger_aggregator_session" && (!ck.HttpOnly || ck.SameSite != http.SameSiteLaxMode) {
			t.Fatalf("session cookie must be HttpOnly + SameSite=Lax: %+v", ck)
		}
	}
	if resp := get(t, c, a.URL+"/api/services", ""); resp.StatusCode != http.StatusOK {
		t.Fatalf("signed in: want 200, got %d", resp.StatusCode)
	}
	if resp := get(t, c, a.URL+"/auth/logout", ""); resp.StatusCode != http.StatusFound {
		t.Fatalf("logout: %d", resp.StatusCode)
	}
	if resp := get(t, c, a.URL+"/api/services", ""); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("after logout: want 401, got %d", resp.StatusCode)
	}
}

func TestUnauthenticatedRequests(t *testing.T) {
	t.Parallel()

	a := newApp(t, newIDP(t), nil)
	c := browser(t)

	// API calls, EventSource and the proxies get 401 (no redirect HTML) ...
	for _, accept := range []string{"", "application/json", "text/event-stream"} {
		resp := get(t, c, a.URL+"/api/services", accept)
		var body map[string]string
		_ = json.NewDecoder(resp.Body).Decode(&body)
		if resp.StatusCode != http.StatusUnauthorized || body["error"] != "unauthenticated" {
			t.Errorf("Accept %q: want 401 JSON, got %d %v", accept, resp.StatusCode, body)
		}
	}
	// ... the health probe works without a session ...
	if resp := get(t, c, a.URL+"/healthz", ""); resp.StatusCode != http.StatusOK {
		t.Errorf("/healthz must stay open, got %d", resp.StatusCode)
	}
	// ... and a tampered cookie is no session.
	req, _ := http.NewRequest(http.MethodGet, a.URL+"/api/services", nil)
	req.AddCookie(&http.Cookie{Name: "swagger_aggregator_session", Value: "forged"})
	if resp, _ := http.DefaultClient.Do(req); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("forged cookie: want 401, got %d", resp.StatusCode)
	}
}

func TestCallbackRejectsWrongStateAndReplay(t *testing.T) {
	t.Parallel()

	a := newApp(t, newIDP(t), nil)
	c := browser(t)

	r := get(t, c, a.URL+"/auth/login", "text/html")
	cb := a.idp.authorize(t, r.Header.Get("Location"))
	bad := strings.Replace(cb, "state=", "state=x", 1)
	if resp := get(t, c, bad, ""); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("wrong state: want 400, got %d", resp.StatusCode)
	}
	// the flow cookie is single use: even the correct callback fails afterwards
	if resp := get(t, c, cb, ""); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("callback without a flow cookie: want 400, got %d", resp.StatusCode)
	}
	if resp := get(t, c, a.URL+"/api/services", ""); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("no session may result: got %d", resp.StatusCode)
	}
}

func TestOnlyLocalReturnPathsAreFollowed(t *testing.T) {
	t.Parallel()

	a := newApp(t, newIDP(t), nil)
	for _, rd := range []string{"https://evil.example/x", "//evil.example/x", "/\\evil.example", "javascript:alert(1)"} {
		c := browser(t)
		r := get(t, c, a.URL+"/auth/login?rd="+url.QueryEscape(rd), "text/html")
		cb := a.idp.authorize(t, r.Header.Get("Location"))
		if resp := get(t, c, cb, ""); resp.Header.Get("Location") != "/" {
			t.Errorf("rd=%q must fall back to /, got %q", rd, resp.Header.Get("Location"))
		}
	}
}

func TestAllowedGroupsAndEmails(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		cfg    func(*auth.Config)
		claims map[string]any
		allow  bool
	}{
		{"no restriction", nil, nil, true},
		{"group matches", func(c *auth.Config) { c.AllowedGroups = []string{"docs"} }, map[string]any{"groups": []string{"x", "docs"}}, true},
		{"group missing", func(c *auth.Config) { c.AllowedGroups = []string{"docs"} }, map[string]any{"groups": []string{"x"}}, false},
		{"no groups claim", func(c *auth.Config) { c.AllowedGroups = []string{"docs"} }, nil, false},
		{"custom groups claim", func(c *auth.Config) { c.AllowedGroups = []string{"docs"}; c.GroupsClaim = "roles" }, map[string]any{"roles": []string{"docs"}}, true},
		{"verified email matches", func(c *auth.Config) { c.AllowedEmails = []string{"A@Example.com"} }, map[string]any{"email": "a@example.com", "email_verified": true}, true},
		{"unverified email is no proof", func(c *auth.Config) { c.AllowedEmails = []string{"a@example.com"} }, map[string]any{"email": "a@example.com", "email_verified": false}, false},
		{"other email", func(c *auth.Config) { c.AllowedEmails = []string{"a@example.com"} }, map[string]any{"email": "b@example.com"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			p := newIDP(t)
			p.claims = tt.claims
			a := newApp(t, p, tt.cfg)
			c := browser(t)
			signIn(t, a, c, "/")
			want := http.StatusUnauthorized
			if tt.allow {
				want = http.StatusOK
			}
			if resp := get(t, c, a.URL+"/api/services", ""); resp.StatusCode != want {
				t.Fatalf("want %d, got %d", want, resp.StatusCode)
			}
		})
	}
}

func TestGroupsFromUserinfoWhenNotInTheIDToken(t *testing.T) {
	t.Parallel()

	p := newIDP(t)
	p.userinfo = map[string]any{"sub": "user-1", "groups": []string{"docs"}}
	a := newApp(t, p, func(c *auth.Config) { c.AllowedGroups = []string{"docs"} })
	c := browser(t)
	signIn(t, a, c, "/")
	if resp := get(t, c, a.URL+"/api/services", ""); resp.StatusCode != http.StatusOK {
		t.Fatalf("groups from userinfo must be honoured, got %d", resp.StatusCode)
	}
}

func TestBasePathRoutesAndCookiesAreScoped(t *testing.T) {
	t.Parallel()

	p := newIDP(t)
	a := newApp(t, p, func(c *auth.Config) {
		c.BasePath = "/docs"
		c.RedirectURL = strings.Replace(c.RedirectURL, "/auth/callback", "/docs/auth/callback", 1)
	})
	c := browser(t)
	r := get(t, c, a.URL+"/docs/status", "text/html")
	if loc := r.Header.Get("Location"); !strings.HasPrefix(loc, "/docs/auth/login?rd=%2Fdocs%2Fstatus") {
		t.Fatalf("login redirect must carry the base path and the original page, got %q", loc)
	}
	r = get(t, c, a.URL+"/docs/auth/login?rd="+url.QueryEscape("/docs/status"), "text/html")
	cb := p.authorize(t, r.Header.Get("Location"))
	if resp := get(t, c, cb, ""); resp.Header.Get("Location") != "/docs/status" {
		t.Fatalf("return path: %q", resp.Header.Get("Location"))
	}
}

func TestConfigValidation(t *testing.T) {
	t.Parallel()

	ok := auth.Config{
		Issuer: "https://idp.example.com", ClientID: "c", ClientSecret: "s", SessionSecret: secret,
		RedirectURL: "https://docs.example.com/auth/callback",
	}
	if _, err := auth.New(ok, zap.NewNop()); err != nil {
		t.Fatalf("valid config: %v", err)
	}
	for name, mutate := range map[string]func(*auth.Config){
		"issuer":        func(c *auth.Config) { c.Issuer = "idp" },
		"client id":     func(c *auth.Config) { c.ClientID = "" },
		"client secret": func(c *auth.Config) { c.ClientSecret = "" },
		"short secret":  func(c *auth.Config) { c.SessionSecret = "short" },
		"redirect path": func(c *auth.Config) { c.RedirectURL = "https://docs.example.com/callback" },
		"redirect abs":  func(c *auth.Config) { c.RedirectURL = "/auth/callback" },
		"base mismatch": func(c *auth.Config) { c.BasePath = "/docs" },
	} {
		c := ok
		mutate(&c)
		if _, err := auth.New(c, zap.NewNop()); err == nil {
			t.Errorf("%s: want an error", name)
		}
	}
}

func TestProviderDownAnswers503NotCrash(t *testing.T) {
	t.Parallel()

	p := newIDP(t)
	a := newApp(t, p, nil)
	p.Close() // identity provider unreachable
	resp := get(t, browser(t), a.URL+"/auth/login", "text/html")
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("want 503, got %d", resp.StatusCode)
	}
}
