package mcptools_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/perryprojects/docker-swagger-aggregator/internal/mcptools"
)

const token = "0123456789abcdef0123456789abcdef-token"

type fakeBackend struct {
	services []mcptools.Service
	specs    map[string]string
}

func (f fakeBackend) Services(context.Context) ([]mcptools.Service, error) { return f.services, nil }
func (f fakeBackend) Spec(_ context.Context, name string) ([]byte, error) {
	if s, ok := f.specs[name]; ok {
		return []byte(s), nil
	}
	for _, svc := range f.services {
		if svc.Name == name {
			return nil, mcptools.ErrNoSpec
		}
	}
	return nil, mcptools.ErrUnknown
}

func backend() fakeBackend {
	return fakeBackend{
		services: []mcptools.Service{
			{Name: "orders", Status: "up", SpecHash: "a1", Group: "shop", Title: "Orders API", Version: "2", Endpoints: 3},
			{Name: "billing", Status: "down", SpecHash: "b1"},
			{Name: "ghost", Status: "down", Error: "no network"},
		},
		specs: map[string]string{
			"orders": `{"openapi":"3.0.3","paths":{
				"/orders":{"get":{"summary":"List orders","operationId":"listOrders","tags":["orders"]},"post":{"summary":"Create an order"}},
				"/orders/{id}":{"get":{"summary":"Read one order","description":"Returns the invoice reference too"}}}}`,
			"billing": `{"swagger":"2.0","paths":{"/invoices":{"get":{"summary":"List invoices","tags":["billing"]}}}}`,
		},
	}
}

func connect(t *testing.T, b mcptools.Backend) *mcp.ClientSession {
	t.Helper()
	ctx := context.Background()
	ct, st := mcp.NewInMemoryTransports()
	if _, err := mcptools.NewServer(b, "test").Connect(ctx, st, nil); err != nil {
		t.Fatal(err)
	}
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}

func call(t *testing.T, cs *mcp.ClientSession, tool string, args any) (*mcp.CallToolResult, map[string]any) {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: tool, Arguments: args})
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if res.StructuredContent != nil {
		b, _ := json.Marshal(res.StructuredContent)
		_ = json.Unmarshal(b, &out)
	}
	return res, out
}

func text(res *mcp.CallToolResult) string {
	var sb strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			sb.WriteString(tc.Text)
		}
	}
	return sb.String()
}

func TestToolsAreRegisteredReadOnly(t *testing.T) {
	t.Parallel()

	cs := connect(t, backend())
	list, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, tool := range list.Tools {
		got[tool.Name] = true
		if tool.Annotations == nil || !tool.Annotations.ReadOnlyHint {
			t.Errorf("%s must be annotated read-only", tool.Name)
		}
	}
	for _, name := range []string{"list_services", "list_endpoints", "search_endpoints", "get_spec"} {
		if !got[name] {
			t.Errorf("tool %s missing; have %v", name, got)
		}
	}
	if len(list.Tools) != 4 {
		t.Errorf("only the four read-only tools may exist, got %d", len(list.Tools))
	}
}

func TestListServicesReportsSpecAndProblem(t *testing.T) {
	t.Parallel()

	_, out := call(t, connect(t, backend()), "list_services", map[string]any{})
	svcs := out["services"].([]any) //nolint:forcetypeassert // test
	if len(svcs) != 3 {
		t.Fatalf("services: %v", svcs)
	}
	orders := svcs[0].(map[string]any) //nolint:forcetypeassert // test
	if orders["title"] != "Orders API" || orders["version"] != "2" || orders["endpoints"] != float64(3) {
		t.Fatalf("orders: %v", orders)
	}
	ghost := svcs[2].(map[string]any) //nolint:forcetypeassert // test
	if ghost["name"] != "ghost" || ghost["hasSpec"] != false || ghost["problem"] != "no network" {
		t.Fatalf("ghost: %v", ghost)
	}
}

func TestListEndpointsAndGetSpec(t *testing.T) {
	t.Parallel()

	cs := connect(t, backend())
	_, out := call(t, cs, "list_endpoints", map[string]any{"service": "orders"})
	eps := out["endpoints"].([]any) //nolint:forcetypeassert // test
	if len(eps) != 3 {
		t.Fatalf("endpoints: %v", eps)
	}
	first := eps[0].(map[string]any) //nolint:forcetypeassert // test
	if first["method"] != "GET" || first["path"] != "/orders" || first["operationId"] != "listOrders" {
		t.Fatalf("ordered by path then method: %v", first)
	}

	res, _ := call(t, cs, "get_spec", map[string]any{"service": "billing"})
	if res.IsError || !strings.Contains(text(res), `"swagger":"2.0"`) {
		t.Fatalf("get_spec: %v %q", res.IsError, text(res))
	}

	for service, want := range map[string]string{"nope": "unknown service", "ghost": "no spec yet"} {
		res, _ = call(t, cs, "get_spec", map[string]any{"service": service})
		if !res.IsError || !strings.Contains(text(res), want) {
			t.Errorf("%s: want an error containing %q, got %v %q", service, want, res.IsError, text(res))
		}
	}
}

func TestSearchEndpoints(t *testing.T) {
	t.Parallel()

	cs := connect(t, backend())
	_, out := call(t, cs, "search_endpoints", map[string]any{"query": "INVOICE"}) // path/summary/description, any case
	matches := out["matches"].([]any)                                             //nolint:forcetypeassert // test
	var found []string
	for _, m := range matches {
		mm := m.(map[string]any)                                                                        //nolint:forcetypeassert // test
		found = append(found, mm["service"].(string)+" "+mm["method"].(string)+" "+mm["path"].(string)) //nolint:forcetypeassert // test
	}
	want := []string{"orders GET /orders/{id}", "billing GET /invoices"} // description of one, summary of the other
	if len(found) != 2 || found[0] != want[0] || found[1] != want[1] {
		t.Fatalf("matches = %v, want %v", found, want)
	}
	if unavailable, _ := out["unavailable"].([]any); len(unavailable) != 1 || unavailable[0] != "ghost" {
		t.Fatalf("the service without a spec must be reported: %v", out["unavailable"])
	}

	_, out = call(t, cs, "search_endpoints", map[string]any{"query": "order", "limit": 1})
	if len(out["matches"].([]any)) != 1 || out["truncated"] != true { //nolint:forcetypeassert // test
		t.Fatalf("limit: %v", out)
	}
	if res, _ := call(t, cs, "search_endpoints", map[string]any{"query": "  "}); !res.IsError {
		t.Fatal("an empty query is an error")
	}
}

func TestHTTPNeedsTheBearerToken(t *testing.T) {
	t.Parallel()

	if _, err := mcptools.Handler(backend(), "test", "short"); err == nil {
		t.Fatal("a short token must be refused")
	}
	h, err := mcptools.Handler(backend(), "test", token)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	defer srv.Close()

	for name, header := range map[string]string{"none": "", "wrong": "Bearer wrong-token-wrong-token-wrong-token-1", "scheme": "Basic " + token} {
		req, _ := http.NewRequest(http.MethodPost, srv.URL, strings.NewReader(`{}`))
		if header != "" {
			req.Header.Set("Authorization", header)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized || resp.Header.Get("WWW-Authenticate") == "" {
			t.Errorf("%s: want 401 with WWW-Authenticate, got %d", name, resp.StatusCode)
		}
	}

	// a real client over Streamable HTTP with the token
	ctx := context.Background()
	client := &http.Client{Transport: bearer{token: token}}
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil).
		Connect(ctx, &mcp.StreamableClientTransport{Endpoint: srv.URL, HTTPClient: client, DisableStandaloneSSE: true}, nil)
	if err != nil {
		t.Fatalf("connect over HTTP: %v", err)
	}
	defer cs.Close()
	_, out := call(t, cs, "list_services", map[string]any{})
	if len(out["services"].([]any)) != 3 { //nolint:forcetypeassert // test
		t.Fatalf("over HTTP: %v", out)
	}
}

type bearer struct{ token string }

func (b bearer) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.token)
	return http.DefaultTransport.RoundTrip(r)
}
