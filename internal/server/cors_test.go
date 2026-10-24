package server_test

import (
	"io/fs"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/perryprojects/docker-swagger-aggregator/internal/server"
)

func corsHandler(t *testing.T, origins ...string) http.Handler {
	t.Helper()
	list, err := server.ParseCORSOrigins(origins)
	if err != nil {
		t.Fatal(err)
	}
	inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("INNER")) })
	return server.CORS(list)(inner)
}

func do(h http.Handler, method, host string, headers map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, "http://"+host+"/api/services", nil)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestParseCORSOrigins(t *testing.T) {
	t.Parallel()

	got, err := server.ParseCORSOrigins([]string{" HTTPS://App.Example.com ", "http://localhost:5173", ""})
	if err != nil || !slices.Equal(got, []string{"https://app.example.com", "http://localhost:5173"}) {
		t.Fatalf("got %v, %v", got, err)
	}
	for _, bad := range []string{"*", "null", "app.example.com", "https://app.example.com/", "https://app.example.com/x",
		"https://*.example.com", "ftp://x.example.com", "https://user@app.example.com", "https://a.example.com?x=1"} {
		if _, err := server.ParseCORSOrigins([]string{bad}); err == nil {
			t.Errorf("%q must be refused", bad)
		}
	}
}

func TestListedOriginGetsCredentialedCORSHeaders(t *testing.T) {
	t.Parallel()

	h := corsHandler(t, "https://app.example.com")
	rec := do(h, http.MethodGet, "docs.example.com", map[string]string{"Origin": "https://app.example.com"})
	if rec.Body.String() != "INNER" || rec.Header().Get("Access-Control-Allow-Origin") != "https://app.example.com" ||
		rec.Header().Get("Access-Control-Allow-Credentials") != "true" || !strings.Contains(rec.Header().Get("Vary"), "Origin") ||
		!strings.Contains(rec.Header().Get("Access-Control-Expose-Headers"), "ETag") {
		t.Fatalf("headers: %v body %q", rec.Header(), rec.Body)
	}
	// never a wildcard: the browser would refuse it with credentials, and it would be wrong
	if rec.Header().Get("Access-Control-Allow-Origin") == "*" {
		t.Fatal("wildcard origin")
	}
}

func TestOtherOriginsGetNoCORSHeaders(t *testing.T) {
	t.Parallel()

	h := corsHandler(t, "https://app.example.com")
	for _, origin := range []string{"https://evil.example", "http://app.example.com", "https://app.example.com.evil.example", "null"} {
		rec := do(h, http.MethodGet, "docs.example.com", map[string]string{"Origin": origin})
		if rec.Header().Get("Access-Control-Allow-Origin") != "" || rec.Header().Get("Access-Control-Allow-Credentials") != "" {
			t.Errorf("%s must get no CORS headers: %v", origin, rec.Header())
		}
	}
}

func TestPreflight(t *testing.T) {
	t.Parallel()

	h := corsHandler(t, "https://app.example.com")
	rec := do(h, http.MethodOptions, "docs.example.com", map[string]string{
		"Origin": "https://app.example.com", "Access-Control-Request-Method": "POST",
		"Access-Control-Request-Headers": "Content-Type, X-Evil, If-None-Match",
	})
	if rec.Code != http.StatusNoContent || rec.Body.Len() != 0 {
		t.Fatalf("a preflight is answered at once: %d %q", rec.Code, rec.Body)
	}
	if got := rec.Header().Get("Access-Control-Allow-Headers"); got != "Content-Type, If-None-Match" {
		t.Errorf("only the known headers are allowed, got %q", got)
	}
	if !strings.Contains(rec.Header().Get("Access-Control-Allow-Methods"), "PATCH") || rec.Header().Get("Access-Control-Max-Age") == "" ||
		rec.Header().Get("Access-Control-Allow-Credentials") != "true" {
		t.Errorf("preflight headers: %v", rec.Header())
	}
	rec = do(h, http.MethodOptions, "docs.example.com", map[string]string{"Origin": "https://evil.example", "Access-Control-Request-Method": "POST"})
	if rec.Code != http.StatusForbidden {
		t.Errorf("a preflight from an unlisted origin: want 403, got %d", rec.Code)
	}
}

// With the cookie relaxed to SameSite=None the browser no longer keeps other sites from sending it: the server must.
func TestChangingRequestsFromUnlistedOriginsAreRefused(t *testing.T) {
	t.Parallel()

	h := corsHandler(t, "https://app.example.com")
	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete} {
		rec := do(h, method, "docs.example.com", map[string]string{"Origin": "https://evil.example"})
		if rec.Code != http.StatusForbidden || strings.Contains(rec.Body.String(), "INNER") {
			t.Errorf("%s from an unlisted origin: want 403, got %d %q", method, rec.Code, rec.Body)
		}
	}
	if rec := do(h, http.MethodPost, "docs.example.com", map[string]string{"Origin": "null"}); rec.Code != http.StatusForbidden {
		t.Errorf("Origin: null must be refused, got %d", rec.Code)
	}
	// reads are not refused by the server (the browser keeps the answer from the page without CORS headers)
	if rec := do(h, http.MethodGet, "docs.example.com", map[string]string{"Origin": "https://evil.example"}); rec.Code != http.StatusOK {
		t.Errorf("GET: %d", rec.Code)
	}
}

func TestSameOriginAndNonBrowserRequestsPass(t *testing.T) {
	t.Parallel()

	h := corsHandler(t, "https://app.example.com")
	cases := map[string]map[string]string{
		"the UI itself":                {"Origin": "https://docs.example.com"},
		"no Origin (curl, a server)":   {},
		"behind a proxy that rewrites": {"Origin": "https://docs.example.com", "X-Forwarded-Host": "docs.example.com"},
		"a listed origin":              {"Origin": "https://app.example.com"},
	}
	for name, headers := range cases {
		host := "docs.example.com"
		if name == "behind a proxy that rewrites" {
			host = "web:3000"
		}
		if rec := do(h, http.MethodPost, host, headers); rec.Code != http.StatusOK || rec.Body.String() != "INNER" {
			t.Errorf("%s: want the request to pass, got %d %q", name, rec.Code, rec.Body)
		}
	}
}

func TestWithoutOriginsTheMiddlewareDoesNothing(t *testing.T) {
	t.Parallel()

	h := corsHandler(t) // same origin only: the behaviour before CORS existed
	rec := do(h, http.MethodPost, "docs.example.com", map[string]string{"Origin": "https://evil.example"})
	if rec.Code != http.StatusOK || rec.Header().Get("Vary") != "" {
		t.Fatalf("must be untouched: %d %v", rec.Code, rec.Header())
	}
}

func TestCORSHeadersAlsoOnTheRefusalsOfTheLogin(t *testing.T) {
	t.Parallel()

	// the web handler puts CORS in front of the login, so a 401 reaches the frontend with readable headers
	list, _ := server.ParseCORSOrigins([]string{"https://app.example.com"})
	denyAll := func(http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { http.Error(w, "login", http.StatusUnauthorized) })
	}
	h := server.NewWebHandler(server.WebConfig{
		UI: fstestUI(), API: mustURL("http://127.0.0.1:1"), Protect: denyAll, CORSOrigins: list,
	})
	req := httptest.NewRequest(http.MethodGet, "/api/services", nil)
	req.Header.Set("Origin", "https://app.example.com")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized || rec.Header().Get("Access-Control-Allow-Origin") != "https://app.example.com" {
		t.Fatalf("a 401 must carry the CORS headers: %d %v", rec.Code, rec.Header())
	}
	pre := httptest.NewRequest(http.MethodOptions, "/auth/login", nil)
	pre.Header.Set("Origin", "https://app.example.com")
	pre.Header.Set("Access-Control-Request-Method", "POST")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, pre)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("the preflight must not meet the login: %d", rec.Code)
	}
}

func fstestUI() fs.FS { return fstest.MapFS{"index.html": {Data: []byte("INDEX")}} }

func mustURL(s string) *url.URL { u, _ := url.Parse(s); return u }
