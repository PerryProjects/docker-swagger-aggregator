package docker_test

import (
	"context"
	"maps"
	"net/netip"
	"sync"
	"testing"
	"time"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/events"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/client"
	"go.uber.org/zap"

	"github.com/perryprojects/docker-swagger-aggregator/internal/discovery"
)

func discardLog() *zap.Logger { return zap.NewNop() }

// fakeAPI is an in-memory docker.API: tests set containers and push events.
type fakeAPI struct {
	mu         sync.Mutex
	containers map[string]container.InspectResponse
	listErr    error
	events     chan events.Message
	streamErrs chan error
	sessions   int // number of Events() calls = connection attempts
}

func newFakeAPI() *fakeAPI {
	return &fakeAPI{
		containers: map[string]container.InspectResponse{},
		events:     make(chan events.Message, 16),
		streamErrs: make(chan error, 1),
	}
}

func (f *fakeAPI) set(c container.InspectResponse) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.containers[c.ID] = c
}

func (f *fakeAPI) sessionCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.sessions
}

func (f *fakeAPI) ContainerList(context.Context, client.ContainerListOptions) (client.ContainerListResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.listErr != nil {
		return client.ContainerListResult{}, f.listErr
	}
	var res client.ContainerListResult
	for id := range f.containers {
		res.Items = append(res.Items, container.Summary{ID: id})
	}
	return res, nil
}

func (f *fakeAPI) ContainerInspect(
	_ context.Context,
	id string,
	_ client.ContainerInspectOptions,
) (client.ContainerInspectResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c, ok := f.containers[id]
	if !ok {
		return client.ContainerInspectResult{}, context.DeadlineExceeded // any error: "no such container"
	}
	return client.ContainerInspectResult{Container: c}, nil
}

func (f *fakeAPI) Events(context.Context, client.EventsListOptions) client.EventsResult {
	f.mu.Lock()
	f.sessions++
	f.mu.Unlock()
	return client.EventsResult{Messages: f.events, Err: f.streamErrs}
}

// recorder is a discovery.Handler that remembers the latest state per ID.
type recorder struct {
	mu      sync.Mutex
	applied map[string]discovery.Container
	removed []string
	retain  []map[string]bool
}

func newRecorder() *recorder { return &recorder{applied: map[string]discovery.Container{}} }

func (r *recorder) Apply(_ context.Context, c discovery.Container) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.applied[c.ID] = c
}

func (r *recorder) Remove(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.applied, id)
	r.removed = append(r.removed, id)
}

func (r *recorder) Retain(ids map[string]bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.retain = append(r.retain, ids)
}

func (r *recorder) get(id string) (discovery.Container, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	c, ok := r.applied[id]
	return c, ok
}

func (r *recorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.applied)
}

func (r *recorder) removedIDs() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.removed...)
}

func waitFor(t *testing.T, d time.Duration, cond func() bool) {
	t.Helper()
	for end := time.Now().Add(d); time.Now().Before(end); time.Sleep(5 * time.Millisecond) {
		if cond() {
			return
		}
	}
	t.Fatal("condition not met in", d)
}

// inspect builds an inspect response; labels are given with the default prefix.
func inspect(
	id, name string,
	running bool,
	labels map[string]string,
	opts ...func(*container.InspectResponse),
) container.InspectResponse {
	c := container.InspectResponse{
		ID:              id,
		Name:            "/" + name,
		State:           &container.State{Running: running},
		Config:          &container.Config{Labels: labels},
		NetworkSettings: &container.NetworkSettings{Networks: map[string]*network.EndpointSettings{}},
	}
	for _, o := range opts {
		o(&c)
	}
	return c
}

func onNetwork(name, ip string) func(*container.InspectResponse) {
	return func(c *container.InspectResponse) {
		c.NetworkSettings.Networks[name] = &network.EndpointSettings{IPAddress: netip.MustParseAddr(ip)}
	}
}

func exposing(ports ...string) func(*container.InspectResponse) {
	return func(c *container.InspectResponse) {
		c.Config.ExposedPorts = network.PortSet{}
		for _, p := range ports {
			c.Config.ExposedPorts[network.MustParsePort(p)] = struct{}{}
		}
	}
}

func publishing(containerPort, hostIP, hostPort string) func(*container.InspectResponse) {
	return func(c *container.InspectResponse) {
		c.NetworkSettings.Ports = network.PortMap{
			network.MustParsePort(containerPort): {{HostIP: netip.MustParseAddr(hostIP), HostPort: hostPort}},
		}
	}
}

func healthy(status container.HealthStatus) func(*container.InspectResponse) {
	return func(c *container.InspectResponse) { c.State.Health = &container.Health{Status: status} }
}

func enabled() map[string]string { return map[string]string{"swagger-aggregator.enable": "true"} }

func with(extra map[string]string) map[string]string {
	m := enabled()
	maps.Copy(m, extra)
	return m
}
