// Package fetcher downloads, validates and normalises OpenAPI/Swagger specs.
package fetcher

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
)

// Doer is the part of [http.Client] the fetcher needs. The per-request timeout
// is the Doer's business (http.Client.Timeout).
type Doer interface {
	Do(req *http.Request) (*http.Response, error)
}

// Config holds the dependencies and limits of a Fetcher.
type Config struct {
	Client   Doer
	MaxBytes int64 // larger bodies are rejected
}

// Fetcher performs a single bounded GET of a spec URL.
type Fetcher struct {
	client   Doer
	maxBytes int64
}

// New creates a Fetcher. Config.Client must be set.
func New(cfg Config) *Fetcher {
	return &Fetcher{client: cfg.Client, maxBytes: cfg.MaxBytes}
}

// Fetch returns the raw response body, failing on non-200 or oversized bodies.
func (f *Fetcher) Fetch(ctx context.Context, specURL string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, specURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json, application/yaml;q=0.9, */*;q=0.1")
	resp, err := f.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected status %s", resp.Status)
	}
	var buf bytes.Buffer
	if n := resp.ContentLength; n > 0 {
		if n > f.maxBytes {
			return nil, f.tooLarge()
		}
		buf.Grow(int(n)) // exact allocation instead of doubling
	}
	if _, err = buf.ReadFrom(io.LimitReader(resp.Body, f.maxBytes+1)); err != nil {
		return nil, err
	}
	if int64(buf.Len()) > f.maxBytes {
		return nil, f.tooLarge()
	}
	return buf.Bytes(), nil
}

func (f *Fetcher) tooLarge() error { return fmt.Errorf("spec larger than %d bytes", f.maxBytes) }
