// Package mcptools is a read-only Model Context Protocol server: it lets an AI assistant discover the
// services the aggregator documents and read their OpenAPI specs. It cannot call the services.
package mcptools

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	defaultLimit  = 25
	maxLimit      = 100
	searchWorkers = 4
	minTokenLen   = 32
)

func httpMethods() []string {
	return []string{"get", "put", "post", "delete", "options", "head", "patch", "trace"}
}

// NewServer registers the tools on a new MCP server.
func NewServer(b Backend, version string) *mcp.Server {
	s := mcp.NewServer(&mcp.Implementation{
		Name: "swagger-aggregator", Title: "Swagger Aggregator", Version: version,
		Description: "Discover the documented services of this installation and read their OpenAPI specs.",
	}, nil)
	h := &handlers{b: b}
	ro := &mcp.ToolAnnotations{ReadOnlyHint: true, OpenWorldHint: new(bool)} // reads this installation only
	mcp.AddTool(s, &mcp.Tool{
		Name:        "list_services",
		Annotations: ro,
		Description: "List the services this installation documents, with their status (up/down), group and the last problem. Start here.",
	}, h.listServices)
	mcp.AddTool(s, &mcp.Tool{
		Name: "list_endpoints", Annotations: ro,
		Description: "List the operations (method, path, summary, tags) of one service. Cheaper than get_spec.",
	}, h.listEndpoints)
	mcp.AddTool(s, &mcp.Tool{
		Name:        "search_endpoints",
		Annotations: ro,
		Description: "Search the operations of all services by a word in the service name, path, summary, description, operationId or tags.",
	}, h.searchEndpoints)
	mcp.AddTool(s, &mcp.Tool{
		Name:        "get_spec",
		Annotations: ro,
		Description: "Return the complete OpenAPI/Swagger spec of one service as JSON. It can be large; prefer list_endpoints or search_endpoints first.",
	}, h.getSpec)
	return s
}

// Handler serves the MCP server over Streamable HTTP and requires "Authorization: Bearer <token>".
func Handler(b Backend, version, token string) (http.Handler, error) {
	if len(token) < minTokenLen {
		return nil, fmt.Errorf(
			"the MCP token must be at least %d characters (e.g. `openssl rand -base64 32`)",
			minTokenLen,
		)
	}
	server := NewServer(b, version)
	mcpHandler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server },
		&mcp.StreamableHTTPOptions{Stateless: true})
	return RequireToken(token, mcpHandler), nil
}

// RequireToken lets only requests with the bearer token through.
func RequireToken(token string, next http.Handler) http.Handler {
	want := []byte(token)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok || subtle.ConstantTimeCompare([]byte(got), want) != 1 {
			w.Header().Set("WWW-Authenticate", `Bearer realm="swagger-aggregator"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

type handlers struct{ b Backend }

type serviceInfo struct {
	Name    string `json:"name"              jsonschema:"service name, the key for the other tools"`
	Status  string `json:"status"            jsonschema:"up or down"`
	Group   string `json:"group,omitempty"`
	Title   string `json:"title,omitempty"   jsonschema:"info.title of the spec"`
	Version string `json:"version,omitempty" jsonschema:"info.version of the spec"`
	// Endpoints is the number of operations in the spec: a hint of how big get_spec will be.
	Endpoints int    `json:"endpoints,omitempty" jsonschema:"number of operations in the spec"`
	HasSpec   bool   `json:"hasSpec"             jsonschema:"false: the service never delivered a spec, the other tools cannot read it"`
	Problem   string `json:"problem,omitempty"   jsonschema:"last fetch or discovery problem"`
}

type listServicesOut struct {
	Services []serviceInfo `json:"services"`
}

func (h *handlers) listServices(
	ctx context.Context,
	_ *mcp.CallToolRequest,
	_ struct{},
) (*mcp.CallToolResult, listServicesOut, error) {
	svcs, err := h.b.Services(ctx)
	if err != nil {
		return nil, listServicesOut{}, err
	}
	out := listServicesOut{Services: make([]serviceInfo, 0, len(svcs))}
	for _, s := range svcs {
		out.Services = append(out.Services, serviceInfo{
			Name: s.Name, Status: s.Status, Group: s.Group, Title: s.Title, Version: s.Version, Endpoints: s.Endpoints,
			HasSpec: s.SpecHash != "", Problem: s.Error,
		})
	}
	return nil, out, nil
}

type serviceIn struct {
	Service string `json:"service" jsonschema:"service name as returned by list_services"`
}

func (h *handlers) getSpec(
	ctx context.Context,
	_ *mcp.CallToolRequest,
	in serviceIn,
) (*mcp.CallToolResult, any, error) {
	spec, err := h.b.Spec(ctx, in.Service)
	if err != nil {
		return nil, nil, toolError(in.Service, err)
	}
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: string(spec)}}}, nil, nil
}

// Endpoint is one operation of a spec.
type Endpoint struct {
	Method      string   `json:"method"`
	Path        string   `json:"path"`
	Summary     string   `json:"summary,omitempty"`
	OperationID string   `json:"operationId,omitempty"`
	Tags        []string `json:"tags,omitempty"`
	description string
}

type listEndpointsOut struct {
	Endpoints []Endpoint `json:"endpoints"`
}

func (h *handlers) listEndpoints(
	ctx context.Context,
	_ *mcp.CallToolRequest,
	in serviceIn,
) (*mcp.CallToolResult, listEndpointsOut, error) {
	spec, err := h.b.Spec(ctx, in.Service)
	if err != nil {
		return nil, listEndpointsOut{}, toolError(in.Service, err)
	}
	eps, err := Endpoints(spec)
	if err != nil {
		return nil, listEndpointsOut{}, fmt.Errorf("service %q: %w", in.Service, err)
	}
	return nil, listEndpointsOut{Endpoints: eps}, nil
}

// Endpoints lists the operations of an OpenAPI 3 or Swagger 2.0 spec, ordered by path, then by method.
func Endpoints(spec []byte) ([]Endpoint, error) {
	var doc struct {
		Paths map[string]map[string]json.RawMessage `json:"paths"`
	}
	if err := json.Unmarshal(spec, &doc); err != nil {
		return nil, fmt.Errorf("spec is not valid JSON: %w", err)
	}
	paths := make([]string, 0, len(doc.Paths))
	for p := range doc.Paths {
		paths = append(paths, p)
	}
	slices.Sort(paths)
	var out []Endpoint
	for _, p := range paths {
		for _, m := range httpMethods() {
			raw, ok := doc.Paths[p][m]
			if !ok {
				continue
			}
			var op struct {
				Summary     string   `json:"summary"`
				Description string   `json:"description"`
				OperationID string   `json:"operationId"`
				Tags        []string `json:"tags"`
			}
			if err := json.Unmarshal(raw, &op); err != nil {
				continue // a malformed operation must not hide the others
			}
			out = append(out, Endpoint{
				Method: strings.ToUpper(m), Path: p, Summary: op.Summary, OperationID: op.OperationID,
				Tags: op.Tags, description: op.Description,
			})
		}
	}
	return out, nil
}

type searchIn struct {
	Query string `json:"query"           jsonschema:"word or phrase to look for, case-insensitive"`
	Limit int    `json:"limit,omitempty" jsonschema:"maximum number of matches, default 25, at most 100"`
}

// Match is one search hit.
type Match struct {
	Endpoint

	Service string `json:"service"`
}

type searchOut struct {
	Matches     []Match  `json:"matches"`
	Truncated   bool     `json:"truncated,omitempty"   jsonschema:"more matches exist than were returned"`
	Unavailable []string `json:"unavailable,omitempty" jsonschema:"services that could not be searched (no spec yet, or an error)"`
}

// searchService returns the endpoints of one service that match the lower-case query q.
func (h *handlers) searchService(ctx context.Context, q, name string) ([]Match, error) {
	spec, err := h.b.Spec(ctx, name)
	if err != nil {
		return nil, err
	}
	eps, err := Endpoints(spec)
	if err != nil {
		return nil, err
	}
	var out []Match
	for _, e := range eps {
		if matches(q, name, e) {
			out = append(out, Match{Service: name, Endpoint: e})
		}
	}
	return out, nil
}

func (h *handlers) searchEndpoints(
	ctx context.Context,
	_ *mcp.CallToolRequest,
	in searchIn,
) (*mcp.CallToolResult, searchOut, error) {
	q := strings.ToLower(strings.TrimSpace(in.Query))
	if q == "" {
		return nil, searchOut{}, errors.New("query must not be empty")
	}
	limit := in.Limit
	if limit <= 0 {
		limit = defaultLimit
	}
	limit = min(limit, maxLimit)

	svcs, err := h.b.Services(ctx)
	if err != nil {
		return nil, searchOut{}, err
	}
	type result struct {
		matches []Match
		failed  bool
	}
	results := make([]result, len(svcs))
	var wg sync.WaitGroup
	sem := make(chan struct{}, searchWorkers)
	for i, s := range svcs {
		wg.Go(func() {
			sem <- struct{}{}
			defer func() { <-sem }()
			var searchErr error
			results[i].matches, searchErr = h.searchService(ctx, q, s.Name)
			results[i].failed = searchErr != nil
		})
	}
	wg.Wait()

	out := searchOut{Matches: []Match{}}
	for i, r := range results { // service order = the API's order: stable output
		if r.failed {
			out.Unavailable = append(out.Unavailable, svcs[i].Name)
		}
		for _, m := range r.matches {
			if len(out.Matches) == limit {
				out.Truncated = true
				break
			}
			out.Matches = append(out.Matches, m)
		}
	}
	return nil, out, nil
}

func matches(q, service string, e Endpoint) bool {
	for _, field := range []string{service, e.Path, e.Summary, e.description, e.OperationID, strings.Join(e.Tags, " ")} {
		if strings.Contains(strings.ToLower(field), q) {
			return true
		}
	}
	return false
}

func toolError(service string, err error) error {
	switch {
	case errors.Is(err, ErrUnknown):
		return fmt.Errorf("unknown service %q: call list_services for the names", service)
	case errors.Is(err, ErrNoSpec):
		return fmt.Errorf("service %q has no spec yet (see its problem in list_services)", service)
	}
	return fmt.Errorf("service %q: %w", service, err)
}
