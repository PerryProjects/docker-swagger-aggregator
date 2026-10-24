package apidocs_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"slices"
	"strings"
	"testing"
	"testing/fstest"

	"go.uber.org/zap"
	"go.yaml.in/yaml/v3"
	"golang.org/x/crypto/bcrypt"

	"github.com/perryprojects/docker-swagger-aggregator/internal/apidocs"
	"github.com/perryprojects/docker-swagger-aggregator/internal/auth"
	"github.com/perryprojects/docker-swagger-aggregator/internal/registry"
	"github.com/perryprojects/docker-swagger-aggregator/internal/server"
	"github.com/perryprojects/docker-swagger-aggregator/internal/store"
)

func loadJSON(t *testing.T, h http.Handler, path string) map[string]any {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("%s: %d", path, rec.Code)
	}
	var doc map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatalf("%s is no JSON: %v", path, err)
	}
	return doc
}

func handler(t *testing.T) http.Handler {
	t.Helper()
	h, err := apidocs.Handler()
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func TestDocumentsAreServedAsJSONAndYAML(t *testing.T) {
	t.Parallel()

	h := handler(t)
	oa := loadJSON(t, h, "/spec/openapi.json")
	aa := loadJSON(t, h, "/spec/asyncapi.json")
	if oa["openapi"] != "3.1.0" || aa["asyncapi"] != "3.1.0" {
		t.Fatalf("versions: %v %v", oa["openapi"], aa["asyncapi"])
	}
	for path, ct := range map[string]string{
		"/spec/openapi.json": "application/json", "/spec/asyncapi.json": "application/json",
		"/spec/openapi.yaml": "application/yaml", "/spec/asyncapi.yaml": "application/yaml",
	} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != ct || rec.Body.Len() == 0 {
			t.Errorf("%s: %d %q", path, rec.Code, rec.Header().Get("Content-Type"))
		}
	}
	// the YAML and the JSON describe the same document
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/spec/openapi.yaml", nil))
	var fromYAML map[string]any
	if err := yaml.Unmarshal(rec.Body.Bytes(), &fromYAML); err != nil {
		t.Fatal(err)
	}
	a, _ := json.Marshal(fromYAML)
	b, _ := json.Marshal(oa)
	if !bytes.Equal(a, b) {
		t.Fatal("openapi.yaml and openapi.json differ")
	}
}

// refs collects every "$ref" below v.
func refs(v any, out *[]string) {
	switch t := v.(type) {
	case map[string]any:
		for k, e := range t {
			if s, ok := e.(string); ok && k == "$ref" {
				*out = append(*out, s)
			}
			refs(e, out)
		}
	case []any:
		for _, e := range t {
			refs(e, out)
		}
	}
}

func resolve(doc map[string]any, ref string) bool {
	cur := any(doc)
	for _, part := range strings.Split(strings.TrimPrefix(ref, "#/"), "/") {
		m, ok := cur.(map[string]any)
		if !ok {
			return false
		}
		if cur, ok = m[part]; !ok {
			return false
		}
	}
	return true
}

func TestEveryReferenceResolvesAndOperationIDsAreUnique(t *testing.T) {
	t.Parallel()

	h := handler(t)
	for _, name := range []string{"openapi", "asyncapi"} {
		doc := loadJSON(t, h, "/spec/"+name+".json")
		var rs []string
		refs(doc, &rs)
		if len(rs) == 0 {
			t.Fatalf("%s: no references at all?", name)
		}
		for _, r := range rs {
			if !resolve(doc, r) {
				t.Errorf("%s: dangling %s", name, r)
			}
		}
	}
	seen := map[string]bool{}
	for path, item := range loadJSON(t, h, "/spec/openapi.json")["paths"].(map[string]any) { //nolint:forcetypeassert // test
		for method, op := range item.(map[string]any) { //nolint:forcetypeassert // test
			o, ok := op.(map[string]any)
			if !ok || method == "parameters" {
				continue
			}
			id, _ := o["operationId"].(string)
			if id == "" || seen[id] {
				t.Errorf("%s %s: operationId %q missing or duplicate", method, path, id)
			}
			seen[id] = true
			if _, has := o["responses"]; !has {
				t.Errorf("%s %s has no responses", method, path)
			}
		}
	}
}

// jsonFields lists the JSON property names of a struct.
func jsonFields(v any) []string {
	var out []string
	rt := reflect.TypeOf(v)
	for i := range rt.NumField() {
		if name, _, _ := strings.Cut(rt.Field(i).Tag.Get("json"), ","); name != "" && name != "-" {
			out = append(out, name)
		}
	}
	slices.Sort(out)
	return out
}

func props(doc map[string]any, schema string) []string {
	p := doc["components"].(map[string]any)["schemas"].(map[string]any)[schema].(map[string]any)["properties"].(map[string]any) //nolint:forcetypeassert // test
	out := make([]string, 0, len(p))
	for k := range p {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

func TestServiceSchemaMatchesWhatTheServerSends(t *testing.T) {
	t.Parallel()

	h := handler(t)
	want := jsonFields(registry.Service{})
	for _, name := range []string{"openapi", "asyncapi"} {
		if got := props(loadJSON(t, h, "/spec/"+name+".json"), "Service"); !slices.Equal(got, want) {
			t.Errorf("%s Service properties %v, the server sends %v", name, got, want)
		}
	}
}

// liveStack is the web handler in front of the real API handler with the built-in authentication.
type liveStack struct {
	web     http.Handler
	session *http.Cookie
}

func newLiveStack(t *testing.T) *liveStack {
	t.Helper()
	reg := registry.New(registry.Config{})
	reg.Upsert("orders", "c1", true, "")
	reg.SetSpec("c1", []byte(`{"openapi":"3.0.3","info":{"title":"x","version":"1"},"paths":{}}`))
	apiSrv := httptest.NewServer(server.New(server.Config{Store: reg, Log: zap.NewNop()}).Handler())
	t.Cleanup(apiSrv.Close)
	apiURL, _ := url.Parse(apiSrv.URL)

	db, err := store.OpenMemory(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	local, err := auth.NewLocal(auth.LocalConfig{
		Store: db, SessionSecret: "0123456789abcdef0123456789abcdef-secret", SetupToken: "tok", BcryptCost: bcrypt.MinCost,
	}, zap.NewNop())
	if err != nil {
		t.Fatal(err)
	}
	if err = local.Bootstrap(context.Background(), "", "", ""); err != nil {
		t.Fatal(err)
	}
	web := server.NewWebHandler(server.WebConfig{
		UI: fstest.MapFS{"index.html": {Data: []byte("INDEX")}}, API: apiURL, Protect: local.Wrap,
	})
	s := &liveStack{web: web}
	rec := s.do(http.MethodPost, "/auth/setup", `{"token":"tok","username":"admin","password":"correct horse battery"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("setup: %d %s", rec.Code, rec.Body)
	}
	s.session = rec.Result().Cookies()[0]
	return s
}

func (s *liveStack) do(method, path, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if s.session != nil {
		req.AddCookie(s.session)
	}
	rec := httptest.NewRecorder()
	s.web.ServeHTTP(rec, req)
	return rec
}

func keys(t *testing.T, body []byte) []string {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatalf("not a JSON object: %q", body)
	}
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

func TestAuthSchemasMatchTheRealResponses(t *testing.T) {
	t.Parallel()

	oa := loadJSON(t, handler(t), "/spec/openapi.json")
	s := newLiveStack(t)

	var users []json.RawMessage
	if err := json.Unmarshal(s.do(http.MethodGet, "/auth/users", "").Body.Bytes(), &users); err != nil || len(users) != 1 {
		t.Fatalf("users: %v %d", err, len(users))
	}
	if got, want := keys(t, users[0]), props(oa, "User"); !slices.Equal(got, want) {
		t.Errorf("User: the server sends %v, documented %v", got, want)
	}
	got := keys(t, s.do(http.MethodGet, "/auth/session", "").Body.Bytes())
	for _, k := range got {
		if !slices.Contains(props(oa, "SessionInfo"), k) {
			t.Errorf("session answer has undocumented field %q", k)
		}
	}
	if got := keys(t, s.do(http.MethodPost, "/auth/login", `{"username":"admin","password":"wrong-password"}`).Body.Bytes()); !slices.Equal(got, props(oa, "Error")) {
		t.Errorf("error body keys %v", got)
	}
}

// TestEveryDocumentedOperationExists calls each operation of the OpenAPI document against the real handlers: a
// route that was removed or renamed answers 404 "page not found" or 405, which the document must not promise.
func TestEveryDocumentedOperationExists(t *testing.T) {
	t.Parallel()

	oa := loadJSON(t, handler(t), "/spec/openapi.json")
	s := newLiveStack(t)
	skip := map[string]bool{"/api/events": true, "/auth/logout": true} // a stream never ends; logout would end the session used below
	methods := []string{"get", "post", "put", "patch", "delete"}
	n := 0
	for path, item := range oa["paths"].(map[string]any) { //nolint:forcetypeassert // test
		if skip[path] {
			continue
		}
		concrete := strings.NewReplacer("{id}", "1", "{name}", "orders", "{path}", "x").Replace(path)
		if path == "/api/proxy/{id}/{path}" {
			concrete = "/api/proxy/c1/x"
		}
		if path == "/api/scalar-proxy" {
			concrete += "?scalar_url=http://nowhere.invalid/x"
		}
		for _, m := range methods {
			if _, documented := item.(map[string]any)[m]; !documented { //nolint:forcetypeassert // test
				continue
			}
			body := ""
			if slices.Contains([]string{"post", "put", "patch"}, m) {
				body = `{}`
			}
			rec := s.do(strings.ToUpper(m), concrete, body)
			if rec.Code == http.StatusMethodNotAllowed || strings.Contains(rec.Body.String(), "404 page not found") {
				t.Errorf("%s %s is documented but not served (%d %q)", strings.ToUpper(m), path, rec.Code, strings.TrimSpace(rec.Body.String()))
			}
			n++
		}
	}
	if n < 20 {
		t.Fatalf("only %d operations checked: the walk is broken", n)
	}
}
