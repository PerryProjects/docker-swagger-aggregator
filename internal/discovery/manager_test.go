package discovery_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/perryprojects/docker-swagger-aggregator/internal/discovery"
	"github.com/perryprojects/docker-swagger-aggregator/internal/registry"
	"github.com/perryprojects/docker-swagger-aggregator/internal/status"
)

// fakeFetcher returns scripted bodies; it counts calls.
type fakeFetcher struct {
	mu    sync.Mutex
	body  []byte
	err   error
	calls atomic.Int32
}

func (f *fakeFetcher) Fetch(context.Context, string) ([]byte, error) {
	f.calls.Add(1)
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.body, f.err
}

func (f *fakeFetcher) set(body string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.body, f.err = []byte(body), err
}

type fixture struct {
	reg      *registry.Registry
	fetch    *fakeFetcher
	mgr      *discovery.Manager
	prepared atomic.Int32
}

func newFixture(t *testing.T, interval time.Duration) *fixture {
	t.Helper()
	f := &fixture{reg: registry.New(registry.Config{}), fetch: &fakeFetcher{}}
	f.fetch.set(`{"openapi":"3.0.0"}`, nil)
	f.mgr = discovery.NewManager(discovery.ManagerConfig{
		Store:   f.reg,
		Fetcher: f.fetch,
		Prepare: func(raw []byte, _ string, id string) ([]byte, error) {
			f.prepared.Add(1)
			if len(raw) == 0 {
				return nil, errors.New("empty spec")
			}
			return append([]byte(id+":"), raw...), nil
		},
		Log:      zap.NewNop(),
		Interval: interval,
		MinRetry: 5 * time.Millisecond,
	})
	return f
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	const d = time.Second
	for end := time.Now().Add(d); time.Now().Before(end); time.Sleep(5 * time.Millisecond) {
		if cond() {
			return
		}
	}
	t.Fatal("condition not met in", d)
}

func (f *fixture) only() registry.Service {
	l := f.reg.List()
	if len(l) != 1 {
		return registry.Service{}
	}
	return l[0]
}

func running() discovery.Container {
	return discovery.Container{
		ID:      "c1",
		Name:    "api",
		Running: true,
		BaseURL: "http://x",
		SpecURL: "http://x/openapi.json",
	}
}

func TestRetriesWithBackoffUntilReadyThenGoesDownOnStop(t *testing.T) {
	t.Parallel()

	f := newFixture(t, 50*time.Millisecond)
	f.fetch.set("", errors.New("connection refused")) // the app is not listening yet right after "start"
	ctx := t.Context()

	f.mgr.Apply(ctx, running())
	waitFor(t, func() bool { return f.fetch.calls.Load() >= 3 }) // retried, not given up
	if s := f.only(); s.Status != status.Down || s.Error != "connection refused" {
		t.Fatalf("while failing: %+v", s)
	}

	f.fetch.set(`{"openapi":"3.0.0"}`, nil)
	waitFor(t, func() bool { return f.only().Status == status.Up })
	if spec, _, _ := f.reg.Spec("api"); string(spec) != `c1:{"openapi":"3.0.0"}` {
		t.Fatalf("stored spec %q (Prepare must receive the container id)", spec)
	}

	stopped := running()
	stopped.Running = false
	f.mgr.Apply(ctx, stopped)
	if s := f.only(); s.Status != status.Down || s.SpecHash == "" {
		t.Fatalf("stopped service must stay listed, down, with its last spec: %+v", s)
	}
	calls := f.fetch.calls.Load()
	time.Sleep(150 * time.Millisecond)
	if f.fetch.calls.Load() != calls {
		t.Fatal("polling must stop when the container stops")
	}

	f.mgr.Remove("c1")
	if len(f.reg.List()) != 0 {
		t.Fatal("destroy must remove the service")
	}
}

func TestUnchangedBodyIsNotPreparedAgain(t *testing.T) {
	t.Parallel()

	f := newFixture(t, 10*time.Millisecond)
	ctx := t.Context()
	f.mgr.Apply(ctx, running())
	waitFor(t, func() bool { return f.fetch.calls.Load() >= 5 && f.only().Status == status.Up })
	if n := f.prepared.Load(); n != 1 {
		t.Fatalf("Prepare ran %d times for an unchanged body, want 1", n)
	}

	f.fetch.set(`{"openapi":"3.0.1"}`, nil) // a real change is picked up
	waitFor(t, func() bool { return f.prepared.Load() == 2 })
}

func TestRecoversWithIdenticalBodyAfterAnOutage(t *testing.T) {
	t.Parallel()

	f := newFixture(t, 10*time.Millisecond)
	ctx := t.Context()
	f.mgr.Apply(ctx, running())
	waitFor(t, func() bool { return f.only().Status == status.Up })
	hash := f.only().SpecHash

	f.fetch.set("", errors.New("503"))
	waitFor(t, func() bool { return f.only().Status == status.Down })
	f.fetch.set(`{"openapi":"3.0.0"}`, nil)
	waitFor(t, func() bool { return f.only().Status == status.Up })
	if f.only().SpecHash != hash || f.prepared.Load() != 1 {
		t.Fatalf("hash %s -> %s, prepared %d", hash, f.only().SpecHash, f.prepared.Load())
	}
}

func TestInvalidSpecIsAProblem(t *testing.T) {
	t.Parallel()

	f := newFixture(t, 10*time.Millisecond)
	f.fetch.set("", nil) // Fetch succeeds, Prepare rejects
	ctx := t.Context()
	f.mgr.Apply(ctx, running())
	waitFor(t, func() bool { return f.only().Error == "empty spec" })
	if f.only().Status != status.Down {
		t.Fatalf("%+v", f.only())
	}
}

func TestProblemFromSourceIsReportedWithoutPolling(t *testing.T) {
	t.Parallel()

	f := newFixture(t, 10*time.Millisecond)
	c := discovery.Container{ID: "c1", Name: "api", Running: true, Problem: "no exposed port"}
	f.mgr.Apply(context.Background(), c)
	if s := f.only(); s.Status != status.Down || s.Error != "no exposed port" {
		t.Fatalf("%+v", s)
	}
	time.Sleep(50 * time.Millisecond)
	if n := f.fetch.calls.Load(); n != 0 {
		t.Fatalf("a container with a problem must not be polled, calls=%d", n)
	}
}

func TestHealthOnlyChangeKeepsTheSamePoller(t *testing.T) {
	t.Parallel()

	f := newFixture(t, 20*time.Millisecond)
	ctx := t.Context()
	c := running()
	f.mgr.Apply(ctx, c)
	waitFor(t, func() bool { return f.only().Status == status.Up })

	c.Health = "unhealthy"
	f.mgr.Apply(ctx, c)
	if f.only().Status != status.Down {
		t.Fatal("unhealthy must be down")
	}
	c.Health = "healthy"
	f.mgr.Apply(ctx, c)
	waitFor(t, func() bool { return f.only().Status == status.Up })
}

func TestRetainStopsPollersOfVanishedContainers(t *testing.T) {
	t.Parallel()

	f := newFixture(t, 10*time.Millisecond)
	ctx := t.Context()
	f.mgr.Apply(ctx, running())
	waitFor(t, func() bool { return f.only().Status == status.Up })

	f.mgr.Retain(map[string]bool{})
	if len(f.reg.List()) != 0 {
		t.Fatal("Retain must drop services that are gone")
	}
	calls := f.fetch.calls.Load()
	time.Sleep(100 * time.Millisecond)
	if f.fetch.calls.Load()-calls > 1 {
		t.Fatal("poller kept running after Retain")
	}
}
