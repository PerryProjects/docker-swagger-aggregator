package fetcher_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/perryprojects/docker-swagger-aggregator/internal/fetcher"
)

func prep(t *testing.T, raw, serverURL string, proxy ...string) map[string]any {
	t.Helper()
	proxyPath := ""
	if len(proxy) > 0 {
		proxyPath = proxy[0]
	}
	out, err := fetcher.Prepare([]byte(raw), serverURL, proxyPath)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err = json.Unmarshal(out, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func TestRewriteOpenAPI3(t *testing.T) {
	t.Parallel()
	m := prep(
		t,
		`{"openapi":"3.0.3","servers":[{"url":"http://internal:8000"}],"paths":{}}`,
		"https://api.example.com/v1",
	)
	s := m["servers"].([]any)
	if len(s) != 1 || s[0].(map[string]any)["url"] != "https://api.example.com/v1" {
		t.Fatalf("servers = %v", s)
	}
}

func TestRewriteSwagger2(t *testing.T) {
	t.Parallel()
	m := prep(
		t,
		`{"swagger":"2.0","host":"internal:8000","basePath":"/","schemes":["http"],"paths":{}}`,
		"https://api.example.com/v1/",
	)
	if m["host"] != "api.example.com" || m["basePath"] != "/v1" || m["schemes"].([]any)[0] != "https" {
		t.Fatalf("got %v %v %v", m["host"], m["basePath"], m["schemes"])
	}
}

func TestNoRewriteWithoutServerURL(t *testing.T) {
	t.Parallel()
	m := prep(t, `{"openapi":"3.1.0","servers":[{"url":"/x"}]}`, "")
	if m["servers"].([]any)[0].(map[string]any)["url"] != "/x" {
		t.Fatal("servers must be untouched")
	}
}

func TestYAMLWithIntegerKeys(t *testing.T) {
	t.Parallel()
	m := prep(
		t,
		"openapi: 3.0.0\npaths:\n  /a:\n    get:\n      responses:\n        200:\n          description: ok\n",
		"",
	)
	r := m["paths"].(map[string]any)["/a"].(map[string]any)["get"].(map[string]any)["responses"].(map[string]any)
	if _, ok := r["200"]; !ok {
		t.Fatalf("responses = %v", r)
	}
}

func TestRejectsNonSpecAndBadServerURL(t *testing.T) {
	t.Parallel()
	for _, raw := range []string{`{"hello":"world"}`, `[1,2]`, `<html>`, `{"openapi":"2.5"}`} {
		if _, err := fetcher.Prepare([]byte(raw), "", ""); err == nil {
			t.Errorf("expected error for %q", raw)
		}
	}
	if _, err := fetcher.Prepare([]byte(`{"openapi":"3.0.0"}`), "ftp://x", ""); err == nil {
		t.Error("expected error for bad server-url")
	}
}

func TestCanonicalOutputStable(t *testing.T) {
	t.Parallel()
	a, _ := fetcher.Prepare([]byte(`{"openapi":"3.0.0","a":1,"b":2}`), "", "")
	b, _ := fetcher.Prepare([]byte(`{"b":2,"a":1,"openapi":"3.0.0"}`), "", "")
	if string(a) != string(b) {
		t.Fatalf("%s != %s", a, b)
	}
}

func TestFetchLimitsAndStatus(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/big":
			_, _ = w.Write([]byte(strings.Repeat("x", 100)))
		case "/slow":
			time.Sleep(300 * time.Millisecond)
		case "/404":
			http.NotFound(w, r)
		default:
			w.Write([]byte(`{"openapi":"3.0.0"}`))
		}
	}))
	defer srv.Close()
	f := fetcher.New(fetcher.Config{Client: &http.Client{Timeout: 100 * time.Millisecond}, MaxBytes: 50})
	ctx := context.Background()
	if _, err := f.Fetch(ctx, srv.URL+"/ok"); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"/big", "/slow", "/404"} {
		if _, err := f.Fetch(ctx, srv.URL+p); err == nil {
			t.Errorf("%s: expected error", p)
		}
	}
}

func TestRewriteToProxyKeepsOriginalPrefix(t *testing.T) {
	t.Parallel()
	m := prep(t, `{"openapi":"3.0.3","servers":[{"url":"http://internal:8000/v1/"}]}`, "", "/api/proxy/abc")
	if u := m["servers"].([]any)[0].(map[string]any)["url"]; u != "/api/proxy/abc/v1" {
		t.Fatalf("servers url = %v", u)
	}
	m = prep(t, `{"openapi":"3.0.3","servers":[{"url":"/"}]}`, "", "/api/proxy/abc")
	if u := m["servers"].([]any)[0].(map[string]any)["url"]; u != "/api/proxy/abc" {
		t.Fatalf("servers url = %v", u)
	}
	m = prep(t, `{"openapi":"3.0.3"}`, "", "/api/proxy/abc") // no servers at all
	if u := m["servers"].([]any)[0].(map[string]any)["url"]; u != "/api/proxy/abc" {
		t.Fatalf("servers url = %v", u)
	}
}

func TestRewriteToProxySwagger2(t *testing.T) {
	t.Parallel()
	m := prep(t, `{"swagger":"2.0","host":"internal:8000","basePath":"/api","schemes":["http"]}`, "", "/api/proxy/abc")
	if _, ok := m["host"]; ok || m["schemes"] != nil || m["basePath"] != "/api/proxy/abc/api" {
		t.Fatalf("got host=%v schemes=%v basePath=%v", m["host"], m["schemes"], m["basePath"])
	}
}

func TestServerURLWinsOverProxy(t *testing.T) {
	t.Parallel()
	m := prep(t, `{"openapi":"3.0.3"}`, "https://api.example.com", "/api/proxy/abc")
	if u := m["servers"].([]any)[0].(map[string]any)["url"]; u != "https://api.example.com" {
		t.Fatalf("servers url = %v", u)
	}
}

func TestRewritePathAndOperationServers(t *testing.T) {
	t.Parallel()
	const spec = `{"openapi":"3.0.3","servers":[{"url":"http://a/v1"}],"paths":{
		"/p":{"servers":[{"url":"http://b/p1"}],"get":{"servers":[{"url":"http://c/op"}]},"post":{}}}}`
	urlAt := func(m map[string]any, keys ...string) any {
		var cur any = m
		for _, k := range append(keys, "servers") {
			cur = cur.(map[string]any)[k]
		}
		return cur.([]any)[0].(map[string]any)["url"]
	}
	m := prep(t, spec, "https://api.example.com")
	if urlAt(m, "paths", "/p") != "https://api.example.com" ||
		urlAt(m, "paths", "/p", "get") != "https://api.example.com" {
		t.Fatalf("server-url: %v", m["paths"])
	}
	m = prep(t, spec, "", "/proxy/x")
	if urlAt(m, "paths", "/p") != "/proxy/x/p1" || urlAt(m, "paths", "/p", "get") != "/proxy/x/op" {
		t.Fatalf("proxy: %v", m["paths"])
	}
	if _, has := m["paths"].(map[string]any)["/p"].(map[string]any)["post"].(map[string]any)["servers"]; has {
		t.Fatal("operation without servers must stay without")
	}
}
