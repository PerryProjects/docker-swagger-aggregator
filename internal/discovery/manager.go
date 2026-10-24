package discovery

import (
	"context"
	"crypto/sha256"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/perryprojects/docker-swagger-aggregator/internal/registry"
)

// Store is what the manager needs from the registry.
type Store interface {
	UpsertReplica(name, id, replicaKey string, running bool, health string)
	SetRoute(id string, up registry.Upstream)
	SetMeta(id, group string, order int)
	SetProblem(id, msg string)
	SetSpec(id string, spec []byte)
	Touch(id string)
	Remove(id string)
	Retain(ids map[string]bool)
}

// SpecFetcher downloads a raw spec.
type SpecFetcher interface {
	Fetch(ctx context.Context, specURL string) ([]byte, error)
}

// PrepareFunc validates and normalises a raw spec for the workload with the
// given ID (e.g. fetcher.Prepare plus the proxy path derived from the ID).
type PrepareFunc func(raw []byte, serverURL, id string) ([]byte, error)

// ManagerConfig holds the manager's dependencies.
type ManagerConfig struct {
	Store   Store
	Fetcher SpecFetcher
	Prepare PrepareFunc
	Log     *zap.Logger
	// Interval is the probe interval and the retry backoff cap.
	Interval time.Duration
	// MinRetry is the first retry delay after a failed fetch (default 1s).
	MinRetry time.Duration
}

// Manager applies workload state to the store and runs one fetch/probe loop
// per running workload.
type Manager struct {
	cfg ManagerConfig

	mu      sync.Mutex
	pollers map[string]poller // by ID
}

type poller struct {
	key    string
	cancel context.CancelFunc
}

// NewManager creates a Manager.
func NewManager(cfg ManagerConfig) *Manager {
	if cfg.MinRetry <= 0 {
		cfg.MinRetry = time.Second
	}
	return &Manager{cfg: cfg, pollers: map[string]poller{}}
}

// Apply implements Handler.
func (m *Manager) Apply(ctx context.Context, c Container) {
	m.cfg.Store.UpsertReplica(c.Name, c.ID, c.Replica, c.Running, c.Health)
	m.cfg.Store.SetRoute(
		c.ID,
		registry.Upstream{Base: c.BaseURL, PublicURL: c.ServerURL, KeepPublicPath: c.KeepServerPath},
	)
	m.cfg.Store.SetMeta(c.ID, c.Group, c.Order)

	m.mu.Lock()
	defer m.mu.Unlock()
	key := c.SpecURL + "|" + c.ServerURL
	old, running := m.pollers[c.ID]

	if c.Problem != "" {
		m.cfg.Store.SetProblem(c.ID, c.Problem)
	}
	if !c.Running || c.Problem != "" {
		if running {
			old.cancel()
			delete(m.pollers, c.ID)
		}
		return
	}
	if running && old.key == key {
		return // health-only change: keep polling
	}
	if running {
		old.cancel()
	}
	pctx, cancel := context.WithCancel(ctx)
	m.pollers[c.ID] = poller{key: key, cancel: cancel}
	go m.poll(pctx, c)
}

// Remove implements Handler.
func (m *Manager) Remove(id string) {
	m.mu.Lock()
	if p, ok := m.pollers[id]; ok {
		p.cancel()
		delete(m.pollers, id)
	}
	m.mu.Unlock()
	m.cfg.Store.Remove(id)
}

// Retain implements Handler.
func (m *Manager) Retain(ids map[string]bool) {
	m.mu.Lock()
	for id, p := range m.pollers {
		if !ids[id] {
			p.cancel()
			delete(m.pollers, id)
		}
	}
	m.mu.Unlock()
	m.cfg.Store.Retain(ids)
}

// poll fetches the spec until ctx is cancelled. A "start" event does not mean
// the app is listening, so failures retry with exponential backoff (MinRetry,
// doubling, capped at Interval) and successes repeat every Interval, which
// doubles as the HTTP liveness probe.
func (m *Manager) poll(ctx context.Context, c Container) {
	backoff := m.cfg.MinRetry
	var lastRaw [sha256.Size]byte
	var haveLast bool
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		wait := m.cfg.Interval
		if err := m.fetchOnce(ctx, c, &lastRaw, &haveLast); err != nil {
			if ctx.Err() != nil {
				return
			}
			m.cfg.Log.Debug(
				"spec fetch failed",
				zap.String("service", c.Name),
				zap.String("url", c.SpecURL),
				zap.Error(err),
			)
			m.cfg.Store.SetProblem(c.ID, err.Error())
			wait = min(backoff, m.cfg.Interval)
			backoff = Backoff(backoff, m.cfg.Interval)
		} else {
			backoff = m.cfg.MinRetry
		}
		timer.Reset(wait)
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
	}
}

// fetchOnce fetches and stores the spec. If the body is byte-identical to the
// previous successful one, parsing and canonicalising are skipped (specs can be
// large and are probed every interval).
func (m *Manager) fetchOnce(ctx context.Context, c Container, lastRaw *[sha256.Size]byte, haveLast *bool) error {
	raw, err := m.cfg.Fetcher.Fetch(ctx, c.SpecURL)
	if err != nil {
		return err
	}
	sum := sha256.Sum256(raw)
	if *haveLast && sum == *lastRaw {
		m.cfg.Store.Touch(c.ID)
		return nil
	}
	spec, err := m.cfg.Prepare(raw, c.ServerURL, c.ID)
	if err != nil {
		return err
	}
	m.cfg.Store.SetSpec(c.ID, spec)
	*lastRaw, *haveLast = sum, true
	return nil
}
