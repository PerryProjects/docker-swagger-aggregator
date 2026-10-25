package mcptools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

// maxSpecBytes bounds what the backend reads of one spec (the API itself limits specs to 10 MiB by default).
const (
	maxSpecBytes   = 32 << 20
	maxErrorBody   = 256 // bytes of an error answer that go into the error message
	requestTimeout = 15 * time.Second
)

// Service is what the API reports about a service (GET /api/services).
type Service struct {
	Name      string     `json:"name"`
	Status    string     `json:"status"`
	LastSeen  *time.Time `json:"lastSeen,omitempty"`
	SpecHash  string     `json:"specHash,omitempty"`
	Error     string     `json:"error,omitempty"`
	Group     string     `json:"group,omitempty"`
	Title     string     `json:"title,omitempty"`
	Version   string     `json:"version,omitempty"`
	OpenAPI   string     `json:"openapi,omitempty"`
	Endpoints int        `json:"endpoints,omitempty"`
}

// ErrNoSpec means the service exists but has not delivered a spec yet.
var ErrNoSpec = errors.New("the service has no spec yet")

// ErrUnknown means no service of that name is registered.
var ErrUnknown = errors.New("unknown service")

// Backend is the read-only view of the aggregator the tools need.
type Backend interface {
	Services(ctx context.Context) ([]Service, error)
	// Spec returns the canonical JSON spec of a service.
	Spec(ctx context.Context, name string) ([]byte, error)
}

// HTTPBackend reads from the API server's /api/services endpoints.
type HTTPBackend struct {
	Base   *url.URL
	Client *http.Client // default: a client with a 15 s timeout
}

func (b HTTPBackend) client() *http.Client {
	if b.Client != nil {
		return b.Client
	}
	return &http.Client{Timeout: requestTimeout}
}

func (b HTTPBackend) get(ctx context.Context, path string) (*http.Response, error) {
	u := b.Base.JoinPath(path)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	return b.client().Do(req)
}

func (b HTTPBackend) Services(ctx context.Context) ([]Service, error) {
	resp, err := b.get(ctx, "/api/services")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("api answered %s", resp.Status)
	}
	var out []Service
	if err = json.NewDecoder(io.LimitReader(resp.Body, maxSpecBytes)).Decode(&out); err != nil {
		return nil, fmt.Errorf("decode services: %w", err)
	}
	return out, nil
}

func (b HTTPBackend) Spec(ctx context.Context, name string) ([]byte, error) {
	// JoinPath escapes the name as one path segment
	resp, err := b.get(ctx, "/api/services/"+url.PathEscape(name)+"/spec")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusNotFound:
		body, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
		if string(body) == "unknown service\n" {
			return nil, ErrUnknown
		}
		return nil, ErrNoSpec
	default:
		return nil, fmt.Errorf("api answered %s", resp.Status)
	}
	b2, err := io.ReadAll(io.LimitReader(resp.Body, maxSpecBytes+1))
	if err != nil {
		return nil, err
	}
	if len(b2) > maxSpecBytes {
		return nil, fmt.Errorf("spec larger than %d bytes", maxSpecBytes)
	}
	return b2, nil
}
