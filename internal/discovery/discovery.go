// Package discovery turns workloads reported by a Source (Docker, Kubernetes, ...)
// into registry entries and keeps their specs fresh.
package discovery

import (
	"context"
	"time"
)

// Container is a discovered workload's current state, resolved to a reachable URL.
type Container struct {
	ID      string
	Name    string
	Running bool
	Health  string // runtime health status; "" when there is no health check

	BaseURL   string // http://host:port, empty when Problem is set
	SpecURL   string // empty when Problem is set
	ServerURL string
	Problem   string // e.g. invalid labels or not attached to the network
	Group     string
	Order     int
	// Source says which source found the workload: "docker" or "kubernetes".
	Source string
	// Replica groups workloads that are replicas of one service (same non-empty value = same service),
	// so they show up as a single API. Empty: not a replica.
	Replica string
	// KeepServerPath is Target.KeepServerPath.
	KeepServerPath bool
}

// Handler receives workload state from a Source. *Manager implements it.
type Handler interface {
	// Apply is idempotent; sources call it for every state change.
	Apply(ctx context.Context, c Container)
	// Remove handles a workload that no longer exists.
	Remove(id string)
	// Retain drops everything not in ids (resync after a reconnect).
	Retain(ids map[string]bool)
}

// Source discovers workloads and reports them to a Handler until ctx ends.
type Source interface {
	Run(ctx context.Context, h Handler)
}

// Backoff doubles d up to max.
func Backoff(d, maxD time.Duration) time.Duration { return min(d*backoffFactor, maxD) }

const backoffFactor = 2
