package app_test

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/events"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/client"

	"github.com/perryprojects/docker-swagger-aggregator/internal/app"
)

// fakeDocker is an in-memory docker.API with one container.
type fakeDocker struct {
	mu      sync.Mutex
	c       container.InspectResponse
	evt     chan events.Message
	errs    chan error
	present bool
}

func (f *fakeDocker) ContainerList(context.Context, client.ContainerListOptions) (client.ContainerListResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.present {
		return client.ContainerListResult{}, nil
	}
	return client.ContainerListResult{Items: []container.Summary{{ID: f.c.ID}}}, nil
}

func (f *fakeDocker) ContainerInspect(
	context.Context,
	string,
	client.ContainerInspectOptions,
) (client.ContainerInspectResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return client.ContainerInspectResult{Container: f.c}, nil
}

func (f *fakeDocker) Events(context.Context, client.EventsListOptions) client.EventsResult {
	return client.EventsResult{Messages: f.evt, Err: f.errs}
}

func (f *fakeDocker) setRunning(running bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.c.State.Running = running
}

func getJSON[T any](t *testing.T, url string) T {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	var v T
	if err = json.Unmarshal(body, &v); err != nil {
		t.Fatalf("%s: %v (%s)", url, err, body)
	}
	return v
}

type svc struct {
	Name, Status, SpecHash, Error string
}

func waitFor(t *testing.T, d time.Duration, what string, cond func() bool) {
	t.Helper()
	for end := time.Now().Add(d); time.Now().Before(end); time.Sleep(10 * time.Millisecond) {
		if cond() {
			return
		}
	}
	t.Fatal("not met in ", d, ": ", what)
}

func TestEndToEndWithFakeDocker(t *testing.T) {
	t.Parallel()

	// the workload: a spec with an internal server URL, plus an endpoint to call through the proxy
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/openapi.json":
			_, _ = w.Write(
				[]byte(
					`{"openapi":"3.0.3","info":{"title":"t","version":"1"},"servers":[{"url":"http://internal:8000"}],"paths":{}}`,
				),
			)
		case "/orders":
			_, _ = w.Write([]byte(`[{"id":1}]`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer backend.Close()
	bu, _ := url.Parse(backend.URL)

	fd := &fakeDocker{
		evt:     make(chan events.Message, 8),
		errs:    make(chan error, 1),
		present: true,
		c: container.InspectResponse{
			ID: "c1", Name: "/orders",
			State: &container.State{Running: true},
			Config: &container.Config{Labels: map[string]string{
				"swagger-aggregator.enable": "true", "swagger-aggregator.port": bu.Port(),
			}},
			NetworkSettings: &container.NetworkSettings{Networks: map[string]*network.EndpointSettings{
				"n": {IPAddress: netip.MustParseAddr("127.0.0.1")},
			}},
		},
	}

	cfg := app.DefaultConfig()
	cfg.ProbeInterval = 50 * time.Millisecond
	cfg.ShutdownGrace = 200 * time.Millisecond
	a, err := app.New(cfg, app.Deps{Docker: fd})
	if err != nil {
		t.Fatal(err)
	}
	ln, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan error, 1)
	go func() { served <- a.Serve(ctx, ln) }()
	base := "http://" + ln.Addr().String()

	// discovered and up
	waitFor(t, 3*time.Second, "service up", func() bool {
		l := getJSON[[]svc](t, base+"/api/services")
		return len(l) == 1 && l[0].Name == "orders" && l[0].Status == "up"
	})

	// the served spec points the browser at the same-origin proxy, keeping no internal address
	spec := getJSON[map[string]any](t, base+"/api/services/orders/spec")
	servers, _ := spec["servers"].([]any)
	if len(servers) != 1 || servers[0].(map[string]any)["url"] != "/api/proxy/c1" { //nolint:forcetypeassert // test
		t.Fatalf("servers = %v", servers)
	}

	// and the proxy reaches the workload
	if got := getJSON[[]map[string]int](t, base+"/api/proxy/c1/orders"); got[0]["id"] != 1 {
		t.Fatalf("proxied body: %v", got)
	}

	// stop -> down, spec kept
	fd.setRunning(false)
	fd.evt <- events.Message{Type: events.ContainerEventType, Action: events.ActionDie, Actor: events.Actor{ID: "c1"}}
	waitFor(t, 3*time.Second, "service down", func() bool {
		l := getJSON[[]svc](t, base+"/api/services")
		return len(l) == 1 && l[0].Status == "down" && l[0].SpecHash != "" && l[0].Error == ""
	})
	if resp, e := http.Get(base + "/api/proxy/c1/orders"); e == nil {
		resp.Body.Close()
		if resp.StatusCode == http.StatusOK {
			t.Fatal("proxy must not forward to a stopped container")
		}
	}

	// destroy -> removed
	fd.evt <- events.Message{Type: events.ContainerEventType, Action: events.ActionDestroy, Actor: events.Actor{ID: "c1"}}
	waitFor(
		t,
		3*time.Second,
		"service removed",
		func() bool { return len(getJSON[[]svc](t, base+"/api/services")) == 0 },
	)

	// graceful shutdown: Serve returns nil and promptly
	cancel()
	select {
	case err = <-served:
		if err != nil {
			t.Fatalf("shutdown: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Serve did not return after cancel")
	}
}

func TestKubernetesOnlyConfigNeedsNoDockerAPI(t *testing.T) {
	t.Parallel()

	// Inside a cluster there is no Docker socket: with docker disabled New must not try to create a client.
	cfg := app.DefaultConfig()
	cfg.DockerEnabled = false
	cfg.Kubernetes.Enabled = true
	cfg.Kubernetes.Kubeconfig = "/nonexistent/kubeconfig"
	if _, err := app.New(cfg, app.Deps{}); err == nil {
		t.Fatal("a missing kubeconfig must be reported, not ignored")
	}
}
