package auth_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"
	"golang.org/x/crypto/bcrypt"

	"github.com/perryprojects/docker-swagger-aggregator/internal/auth"
	"github.com/perryprojects/docker-swagger-aggregator/internal/store"
)

const setupToken = "setup-token-for-tests"

type localEnv struct {
	*httptest.Server

	store *store.Store
	local *auth.Local
}

func newLocal(t *testing.T, bootstrap bool) *localEnv {
	return newLocalWith(t, bootstrap, time.Millisecond)
}

// newLocalWith sets the failed-attempt delay: tiny so that tests can retry at once, or long to see the throttle.
func newLocalWith(t *testing.T, bootstrap bool, throttle time.Duration) *localEnv {
	t.Helper()
	s, err := store.OpenMemory(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	l, err := auth.NewLocal(auth.LocalConfig{
		Store: s, SessionSecret: secret, SetupToken: setupToken, BcryptCost: bcrypt.MinCost, ThrottleBase: throttle,
	}, zap.NewNop())
	if err != nil {
		t.Fatal(err)
	}
	if bootstrap {
		if err = l.Bootstrap(context.Background(), "", "", ""); err != nil {
			t.Fatal(err)
		}
	}
	app := http.NewServeMux()
	app.HandleFunc("/api/services", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("[]")) })
	app.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) })
	app.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("INDEX")) })
	srv := httptest.NewServer(l.Wrap(app))
	t.Cleanup(srv.Close)
	return &localEnv{Server: srv, store: s, local: l}
}

type client struct {
	t *testing.T
	e *localEnv
	c *http.Client
}

func (e *localEnv) client(t *testing.T) *client {
	jar, _ := cookiejar.New(nil)
	return &client{t: t, e: e, c: &http.Client{Jar: jar}}
}

func (c *client) do(method, path string, body any) (int, map[string]any) {
	c.t.Helper()
	var rd *bytes.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	} else {
		rd = bytes.NewReader(nil)
	}
	req, _ := http.NewRequest(method, c.e.URL+path, rd)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.c.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func (c *client) status(path string) int {
	c.t.Helper()
	code, _ := c.do(http.MethodGet, path, nil)
	return code
}

func (c *client) mustDo(want int, method, path string, body any) map[string]any {
	c.t.Helper()
	code, out := c.do(method, path, body)
	if code != want {
		c.t.Fatalf("%s %s: want %d, got %d %v", method, path, want, code, out)
	}
	return out
}

// admin returns a client signed in as the first admin (via setup).
func (e *localEnv) admin(t *testing.T) *client {
	c := e.client(t)
	c.mustDo(http.StatusCreated, "POST", "/auth/setup",
		map[string]any{"token": setupToken, "username": "Admin", "password": "correct horse battery"})
	return c
}

func TestSetupCreatesTheFirstAdminOnceWithTheToken(t *testing.T) {
	t.Parallel()

	e := newLocal(t, true)
	c := e.client(t)
	if info := c.mustDo(200, "GET", "/auth/session", nil); info["setupRequired"] != true || info["user"] != nil {
		t.Fatalf("fresh instance asks for setup: %v", info)
	}
	body := map[string]any{"token": setupToken, "username": "admin", "password": "short"}
	c.mustDo(http.StatusBadRequest, "POST", "/auth/setup", body)
	body["password"] = "correct horse battery"
	body["username"] = "bad name!"
	c.mustDo(http.StatusBadRequest, "POST", "/auth/setup", body)

	body["username"] = "Admin"
	user := c.mustDo(http.StatusCreated, "POST", "/auth/setup", body)["user"].(map[string]any) //nolint:forcetypeassert // test
	if user["username"] != "admin" || user["role"] != "admin" || user["passwordHash"] != nil {
		t.Fatalf("user: %v", user)
	}
	if c.status("/api/services") != 200 {
		t.Fatal("setup signs the new admin in")
	}
	c.mustDo(http.StatusConflict, "POST", "/auth/setup", body) // closed for good
	if info := c.mustDo(200, "GET", "/auth/session", nil); info["setupRequired"] != false {
		t.Fatalf("after setup: %v", info)
	}
}

func TestWrongSetupTokenIsRefusedAndThrottled(t *testing.T) {
	t.Parallel()

	e := newLocalWith(t, true, time.Minute)
	c := e.client(t)
	body := map[string]any{"token": "wrong", "username": "admin", "password": "correct horse battery"}
	c.mustDo(http.StatusForbidden, "POST", "/auth/setup", body)
	body["token"] = setupToken // even the right token has to wait after a failure: no guessing at speed
	c.mustDo(http.StatusTooManyRequests, "POST", "/auth/setup", body)
	if n, _ := e.store.CountUsers(context.Background()); n != 0 {
		t.Fatal("no user may be created")
	}
}

func TestWithoutASetupTokenSetupIsRefused(t *testing.T) {
	t.Parallel()

	s, _ := store.OpenMemory(context.Background())
	t.Cleanup(func() { _ = s.Close() })
	l, _ := auth.NewLocal(auth.LocalConfig{Store: s, SessionSecret: secret, BcryptCost: bcrypt.MinCost}, zap.NewNop())
	srv := httptest.NewServer(l.Wrap(http.NotFoundHandler())) // Bootstrap not called: no token exists at all
	defer srv.Close()
	e := &localEnv{Server: srv, store: s, local: l}
	e.client(t).mustDo(http.StatusForbidden, "POST", "/auth/setup",
		map[string]any{"token": "", "username": "admin", "password": "correct horse battery"})
}

func TestAPIRequiresASessionButTheUIAndHealthzDoNot(t *testing.T) {
	t.Parallel()

	e := newLocal(t, true)
	c := e.client(t)
	for _, p := range []string{"/api/services", "/api/events", "/api/proxy/c1/x"} {
		if code := c.status(p); code != http.StatusUnauthorized {
			t.Errorf("%s without a session: want 401, got %d", p, code)
		}
	}
	for _, p := range []string{"/", "/status", "/assets/x.js", "/healthz"} {
		if code := c.status(p); code != http.StatusOK {
			t.Errorf("%s must stay public (the login form lives in it), got %d", p, code)
		}
	}
	c = e.admin(t)
	if c.status("/api/services") != 200 {
		t.Fatal("signed in: API reachable")
	}
	c.mustDo(http.StatusNoContent, "POST", "/auth/logout", nil)
	if c.status("/api/services") != http.StatusUnauthorized {
		t.Fatal("after logout: 401")
	}
}

func TestWrongPasswordsAreThrottledPerUsername(t *testing.T) {
	t.Parallel()

	e := newLocalWith(t, true, time.Minute)
	admin := e.admin(t)
	admin.mustDo(
		http.StatusCreated,
		"POST",
		"/auth/users",
		map[string]any{"username": "carol", "password": "carol-password-1"},
	)

	c := e.client(t)
	c.mustDo(http.StatusUnauthorized, "POST", "/auth/login", map[string]any{"username": "carol", "password": "nope"})
	c.mustDo(
		http.StatusTooManyRequests,
		"POST",
		"/auth/login",
		map[string]any{"username": "carol", "password": "carol-password-1"},
	) // delayed even with the right password
	c.mustDo(
		http.StatusUnauthorized,
		"POST",
		"/auth/login",
		map[string]any{"username": "ghost", "password": "whatever-123"},
	) // unknown user: the same answer as a wrong password
	// another username is not affected
	admin.mustDo(
		http.StatusCreated,
		"POST",
		"/auth/users",
		map[string]any{"username": "dora", "password": "dora-password-1"},
	)
	e.client(t).
		mustDo(http.StatusOK, "POST", "/auth/login", map[string]any{"username": "dora", "password": "dora-password-1"})
}

func TestDisabledUsersCannotLogInAndAreSignedOut(t *testing.T) {
	t.Parallel()

	e := newLocal(t, true)
	admin := e.admin(t)
	admin.mustDo(
		http.StatusCreated,
		"POST",
		"/auth/users",
		map[string]any{"username": "frank", "password": "frank-password-1"},
	)
	frank := e.client(t)
	frank.mustDo(200, "POST", "/auth/login", map[string]any{"username": "frank", "password": "frank-password-1"})

	admin.mustDo(200, "PATCH", "/auth/users/"+userID(t, admin, "frank"), map[string]any{"disabled": true})
	if frank.status("/api/services") != http.StatusUnauthorized {
		t.Fatal("a disabled user's session must end")
	}
	e.client(t).
		mustDo(http.StatusUnauthorized, "POST", "/auth/login", map[string]any{"username": "frank", "password": "frank-password-1"})
}

func userID(t *testing.T, admin *client, name string) string {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, admin.e.URL+"/auth/users", nil)
	resp, err := admin.c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var us []map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&us)
	for _, u := range us {
		if u["username"] == name {
			return strings.TrimSuffix(strings.TrimSpace(jsonNum(u["id"])), ".0")
		}
	}
	t.Fatalf("user %q not listed: %v", name, us)
	return ""
}

func jsonNum(v any) string { b, _ := json.Marshal(v); return string(b) }

func TestUserManagementIsAdminOnlyAndProtectsTheLastAdmin(t *testing.T) {
	t.Parallel()

	e := newLocal(t, true)
	admin := e.admin(t)
	admin.mustDo(
		http.StatusCreated,
		"POST",
		"/auth/users",
		map[string]any{"username": "dave", "password": "dave-password-1", "role": "user"},
	)
	admin.mustDo(
		http.StatusConflict,
		"POST",
		"/auth/users",
		map[string]any{"username": "DAVE", "password": "dave-password-1"},
	)
	admin.mustDo(http.StatusBadRequest, "POST", "/auth/users", map[string]any{"username": "erin", "password": "x"})
	admin.mustDo(
		http.StatusBadRequest,
		"POST",
		"/auth/users",
		map[string]any{"username": "erin", "password": "erin-password-1", "role": "root"},
	)

	dave := e.client(t)
	dave.mustDo(200, "POST", "/auth/login", map[string]any{"username": "dave", "password": "dave-password-1"})
	if dave.status("/api/services") != 200 {
		t.Fatal("a plain user may use the docs")
	}
	if dave.status("/auth/users") != http.StatusForbidden {
		t.Fatal("a plain user must not list users")
	}
	dave.mustDo(
		http.StatusForbidden,
		"POST",
		"/auth/users",
		map[string]any{"username": "x-user", "password": "password-123456"},
	)

	adminID := userID(t, admin, "admin")
	admin.mustDo(http.StatusConflict, "PATCH", "/auth/users/"+adminID, map[string]any{"role": "user"}) // the last admin
	admin.mustDo(http.StatusBadRequest, "DELETE", "/auth/users/"+adminID, nil)                         // not yourself
	daveID := userID(t, admin, "dave")
	admin.mustDo(200, "PATCH", "/auth/users/"+daveID, map[string]any{"role": "admin"})
	admin.mustDo(http.StatusNoContent, "DELETE", "/auth/users/"+daveID, nil)
	admin.mustDo(http.StatusNotFound, "DELETE", "/auth/users/"+daveID, nil)
	if dave.status("/api/services") != http.StatusUnauthorized {
		t.Fatal("a deleted user's session must end")
	}
}

func TestPasswordChangeEndsOtherSessionsButNotTheCurrentOne(t *testing.T) {
	t.Parallel()

	e := newLocal(t, true)
	first := e.admin(t)
	second := e.client(t)
	second.mustDo(200, "POST", "/auth/login", map[string]any{"username": "admin", "password": "correct horse battery"})

	first.mustDo(
		http.StatusForbidden,
		"POST",
		"/auth/password",
		map[string]any{"current": "wrong", "new": "another long password"},
	)
	first.mustDo(
		http.StatusNoContent,
		"POST",
		"/auth/password",
		map[string]any{"current": "correct horse battery", "new": "another long password"},
	)
	if first.status("/api/services") != 200 {
		t.Fatal("the browser that changed the password stays signed in")
	}
	if second.status("/api/services") != http.StatusUnauthorized {
		t.Fatal("other sessions must end")
	}
	fresh := e.client(t)
	fresh.mustDo(200, "POST", "/auth/login", map[string]any{"username": "admin", "password": "another long password"})
}

func TestMutationsNeedJSONAndSmallBodies(t *testing.T) {
	t.Parallel()

	e := newLocal(t, true)
	req, _ := http.NewRequest(http.MethodPost, e.URL+"/auth/login", strings.NewReader("username=a&password=b"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded") // what a cross-site HTML form sends
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnsupportedMediaType {
		t.Fatalf("form post: want 415, got %d", resp.StatusCode)
	}
	big := strings.Repeat("a", 20<<10)
	req, _ = http.NewRequest(
		http.MethodPost,
		e.URL+"/auth/login",
		strings.NewReader(`{"username":"`+big+`","password":"x"}`),
	)
	req.Header.Set("Content-Type", "application/json")
	resp, _ = http.DefaultClient.Do(req)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("oversized body: want 400, got %d", resp.StatusCode)
	}
}

func TestBootstrapFromEnvironment(t *testing.T) {
	t.Parallel()

	e := newLocal(t, false)
	ctx := context.Background()
	if err := e.local.Bootstrap(ctx, "Root", "environment-pass-1", ""); err != nil {
		t.Fatal(err)
	}
	c := e.client(t)
	if info := c.mustDo(200, "GET", "/auth/session", nil); info["setupRequired"] != false {
		t.Fatalf("an admin from the environment replaces the setup: %v", info)
	}
	c.mustDo(200, "POST", "/auth/login", map[string]any{"username": "root", "password": "environment-pass-1"})

	// the environment never overwrites an existing account
	if err := e.local.Bootstrap(ctx, "root", "a-different-password-2", ""); err != nil {
		t.Fatal(err)
	}
	e.client(t).mustDo(200, "POST", "/auth/login", map[string]any{"username": "root", "password": "environment-pass-1"})

	// a bcrypt hash instead of a plaintext password; short/invalid values are refused
	e2 := newLocal(t, false)
	hash, _ := bcrypt.GenerateFromPassword([]byte("hashed-pass-123"), bcrypt.MinCost)
	if err := e2.local.Bootstrap(ctx, "ops", "", string(hash)); err != nil {
		t.Fatal(err)
	}
	e2.client(t).mustDo(200, "POST", "/auth/login", map[string]any{"username": "ops", "password": "hashed-pass-123"})
	e3 := newLocal(t, false)
	if err := e3.local.Bootstrap(ctx, "ops", "short", ""); err == nil {
		t.Fatal("a short password must be refused")
	}
	if err := e3.local.Bootstrap(ctx, "ops", "", "not-a-hash"); err == nil {
		t.Fatal("a non-bcrypt hash must be refused")
	}
}

func TestSessionCookieSameSiteIsConfigurable(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct {
		sameSite   http.SameSite
		want       http.SameSite
		wantSecure bool
	}{
		"default is Lax and not Secure on plain http": {0, http.SameSiteLaxMode, false},
		"None needs (and sets) Secure":                {http.SameSiteNoneMode, http.SameSiteNoneMode, true},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			s, err := store.OpenMemory(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = s.Close() })
			l, err := auth.NewLocal(auth.LocalConfig{
				Store:         s,
				SessionSecret: secret,
				SetupToken:    setupToken,
				BcryptCost:    bcrypt.MinCost,
				SameSite:      tc.sameSite,
			}, zap.NewNop())
			if err != nil {
				t.Fatal(err)
			}
			srv := httptest.NewServer(l.Wrap(http.NotFoundHandler()))
			defer srv.Close()
			if err = l.Bootstrap(context.Background(), "", "", ""); err != nil {
				t.Fatal(err)
			}
			body := `{"token":"` + setupToken + `","username":"admin","password":"correct horse battery"}`
			resp, err := http.Post(srv.URL+"/auth/setup", "application/json", strings.NewReader(body))
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			cookies := resp.Cookies()
			if len(cookies) != 1 || cookies[0].SameSite != tc.want || cookies[0].Secure != tc.wantSecure ||
				!cookies[0].HttpOnly {
				t.Fatalf("session cookie: %+v", cookies)
			}
		})
	}
}

// sse reads "event: x / data: y" blocks from the user event stream into a channel; it closes when the stream ends.
func sse(t *testing.T, c *client) (<-chan [2]string, int) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, c.e.URL+"/auth/users/events", nil)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	resp, err := c.c.Do(req.WithContext(ctx))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, resp.StatusCode
	}
	out := make(chan [2]string, 16)
	go func() {
		defer close(out)
		defer resp.Body.Close()
		var event string
		sc := bufio.NewScanner(resp.Body)
		sc.Buffer(make([]byte, 64<<10), 1<<20)
		for sc.Scan() {
			line := sc.Text()
			switch {
			case strings.HasPrefix(line, "event: "):
				event = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				out <- [2]string{event, strings.TrimPrefix(line, "data: ")}
			}
		}
	}()
	return out, resp.StatusCode
}

func next(t *testing.T, ch <-chan [2]string) (string, bool) {
	t.Helper()
	select {
	case ev, ok := <-ch:
		return ev[1], ok
	case <-time.After(3 * time.Second):
		t.Fatal("no event within 3 s")
		return "", false
	}
}

func TestUserEventStreamIsAdminOnlyAndFollowsChanges(t *testing.T) {
	t.Parallel()

	e := newLocal(t, true)
	admin := e.admin(t)
	admin.mustDo(
		http.StatusCreated,
		"POST",
		"/auth/users",
		map[string]any{"username": "gina", "password": "gina-password-1"},
	)

	if _, status := sse(t, e.client(t)); status != http.StatusUnauthorized {
		t.Fatalf("no session: want 401, got %d", status)
	}
	gina := e.client(t)
	gina.mustDo(200, "POST", "/auth/login", map[string]any{"username": "gina", "password": "gina-password-1"})
	if _, status := sse(t, gina); status != http.StatusForbidden {
		t.Fatalf("a plain user: want 403, got %d", status)
	}

	ch, status := sse(t, admin)
	if status != http.StatusOK {
		t.Fatalf("admin: %d", status)
	}
	first, _ := next(t, ch)
	if !strings.Contains(first, `"username":"admin"`) || !strings.Contains(first, `"username":"gina"`) ||
		strings.Contains(first, "passwordHash") {
		t.Fatalf("the first event is the whole list: %s", first)
	}

	// another browser of the same admin changes something: the stream shows it
	other := e.client(t)
	other.mustDo(200, "POST", "/auth/login", map[string]any{"username": "admin", "password": "correct horse battery"})
	other.mustDo(
		http.StatusCreated,
		"POST",
		"/auth/users",
		map[string]any{"username": "hank", "password": "hank-password-1"},
	)
	if ev, _ := next(t, ch); !strings.Contains(ev, `"username":"hank"`) {
		t.Fatalf("created user missing: %s", ev)
	}
	other.mustDo(200, "PATCH", "/auth/users/"+userID(t, other, "hank"), map[string]any{"disabled": true})
	if ev, _ := next(t, ch); !strings.Contains(ev, `"username":"hank","role":"user","disabled":true`) {
		t.Fatalf("disabled flag missing: %s", ev)
	}
	other.mustDo(http.StatusNoContent, "DELETE", "/auth/users/"+userID(t, other, "hank"), nil)
	if ev, _ := next(t, ch); strings.Contains(ev, `"username":"hank"`) {
		t.Fatalf("deleted user still listed: %s", ev)
	}
}

func TestUserEventStreamEndsWhenTheAdminIsDemoted(t *testing.T) {
	t.Parallel()

	e := newLocal(t, true)
	root := e.admin(t)
	root.mustDo(
		http.StatusCreated,
		"POST",
		"/auth/users",
		map[string]any{"username": "ivy", "password": "ivy-password-12", "role": "admin"},
	)
	ivy := e.client(t)
	ivy.mustDo(200, "POST", "/auth/login", map[string]any{"username": "ivy", "password": "ivy-password-12"})
	ch, status := sse(t, ivy)
	if status != http.StatusOK {
		t.Fatalf("status %d", status)
	}
	next(t, ch) // the first snapshot

	root.mustDo(200, "PATCH", "/auth/users/"+userID(t, root, "ivy"), map[string]any{"role": "user"})
	if _, open := next(t, ch); open {
		t.Fatal("a demoted admin must not keep receiving the user list")
	}
}
