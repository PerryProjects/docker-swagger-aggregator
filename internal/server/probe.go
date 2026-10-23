package server

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"
)

const probeTimeout = 3 * time.Second

// Doer is the part of [http.Client] Probe needs.
type Doer interface {
	Do(req *http.Request) (*http.Response, error)
}

// Probe GETs /healthz of a server listening on listen (":8080" or "host:port")
// and fails unless it answers 200. It backs the `healthcheck` subcommands,
// because the distroless image has no shell, wget or curl.
func Probe(ctx context.Context, client Doer, listen string) error {
	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		return fmt.Errorf("invalid listen address %q: %w", listen, err)
	}
	if host == "" || strings.Trim(host, "[]") == "::" || host == "0.0.0.0" {
		host = "127.0.0.1"
	}
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+net.JoinHostPort(host, port)+"/healthz", nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("healthz returned %s", resp.Status)
	}
	return nil
}
