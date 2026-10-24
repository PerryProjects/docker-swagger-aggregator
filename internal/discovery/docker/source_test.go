package docker_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/events"

	"github.com/perryprojects/docker-swagger-aggregator/internal/discovery/docker"
)

func run(t *testing.T, api docker.API, cfg docker.Config) *recorder {
	t.Helper()
	cfg.ReconnectBackoff = 10 * time.Millisecond
	rec := newRecorder()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { docker.New(api, cfg, discardLog()).Run(ctx, rec); close(done) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Error("Run did not stop after cancel")
		}
	})
	return rec
}

func TestResolvesRunningContainerToNetworkIP(t *testing.T) {
	t.Parallel()

	api := newFakeAPI()
	api.set(inspect("c1", "orders", true, with(map[string]string{
		"swagger-aggregator.group": "shop", "swagger-aggregator.order": "2",
		"swagger-aggregator.path": "docs.json",
	}), onNetwork("b", "10.0.0.9"), onNetwork("a", "10.0.0.5"), exposing("9000/tcp", "8000/tcp", "53/udp"), healthy(container.Healthy)))
	rec := run(t, api, docker.Config{})

	waitFor(t, time.Second, func() bool { _, ok := rec.get("c1"); return ok })
	c, _ := rec.get("c1")
	want := "http://10.0.0.5:8000" // first network by name, lowest exposed tcp port
	if c.Name != "orders" || c.BaseURL != want || c.SpecURL != want+"/docs.json" || !c.Running ||
		c.Health != "healthy" || c.Group != "shop" || c.Order != 2 || c.Problem != "" {
		t.Fatalf("got %+v", c)
	}
}

func TestPreferredNetworkAndMissingNetwork(t *testing.T) {
	t.Parallel()

	api := newFakeAPI()
	api.set(
		inspect(
			"c1",
			"a",
			true,
			enabled(),
			exposing("80/tcp"),
			onNetwork("front", "10.1.0.1"),
			onNetwork("back", "10.2.0.1"),
		),
	)
	api.set(inspect("c2", "b", true, enabled(), exposing("80/tcp"), onNetwork("front", "10.1.0.2")))
	rec := run(t, api, docker.Config{Network: "back"})

	waitFor(t, time.Second, func() bool { return rec.count() == 2 })
	if c, _ := rec.get("c1"); c.BaseURL != "http://10.2.0.1:80" {
		t.Errorf("preferred network ignored: %+v", c)
	}
	if c, _ := rec.get("c2"); c.Problem != `container is not attached to network "back"` || c.SpecURL != "" {
		t.Errorf("missing network must be reported as a problem: %+v", c)
	}
}

func TestStoppedContainerIsDownWithoutProblem(t *testing.T) {
	t.Parallel()

	api := newFakeAPI()
	// A stopped container has no network settings on a real daemon.
	api.set(inspect("c1", "orders", false, enabled(), exposing("8000/tcp")))
	rec := run(t, api, docker.Config{Network: "back"})

	waitFor(t, time.Second, func() bool { _, ok := rec.get("c1"); return ok })
	if c, _ := rec.get("c1"); c.Running || c.Problem != "" {
		t.Fatalf("got %+v", c)
	}
}

func TestInvalidLabelsAreShownAsProblem_NotEnabledIsIgnored(t *testing.T) {
	t.Parallel()

	api := newFakeAPI()
	api.set(
		inspect(
			"bad",
			"bad",
			true,
			with(map[string]string{"swagger-aggregator.port": "99999"}),
			onNetwork("n", "10.0.0.1"),
		),
	)
	api.set(inspect("off", "off", true, map[string]string{"swagger-aggregator.enable": "false"}))
	api.set(inspect("broken", "broken", true, enabled(), func(c *container.InspectResponse) { c.Config = nil }))
	rec := run(t, api, docker.Config{})

	waitFor(t, time.Second, func() bool { _, ok := rec.get("bad"); return ok })
	if c, _ := rec.get("bad"); c.Problem == "" || c.Name != "bad" {
		t.Errorf("invalid labels must surface as a problem: %+v", c)
	}
	time.Sleep(50 * time.Millisecond)
	if _, ok := rec.get("off"); ok {
		t.Error("a container with enable=false must be ignored")
	}
	if _, ok := rec.get("broken"); ok {
		t.Error("an incomplete inspect response must be skipped")
	}
}

func TestInvalidLabelsKeepTheServiceName(t *testing.T) {
	t.Parallel()

	api := newFakeAPI()
	api.set(
		inspect(
			"c1",
			"proj-orders-1",
			true,
			with(map[string]string{"com.docker.compose.service": "orders"}),
			onNetwork("n", "10.0.0.1"),
		),
	) // no port
	api.set(inspect("c2", "proj-x-1", true, with(map[string]string{
		"com.docker.compose.service": "x", "swagger-aggregator.name": "Pretty X", "swagger-aggregator.order": "oops",
	}), onNetwork("n", "10.0.0.2"), exposing("80/tcp")))
	rec := run(t, api, docker.Config{})
	waitFor(t, time.Second, func() bool { return rec.count() == 2 })
	for id, want := range map[string]string{"c1": "orders", "c2": "Pretty X"} {
		if c, _ := rec.get(id); c.Name != want || c.Problem == "" {
			t.Errorf("%s: name %q, problem %q; want name %q with a problem", id, c.Name, c.Problem, want)
		}
	}
}

func TestComposeServiceNameBeatsContainerName(t *testing.T) {
	t.Parallel()

	api := newFakeAPI()
	api.set(inspect("c1", "proj-billing-1", true, with(map[string]string{"com.docker.compose.service": "billing"}),
		exposing("80/tcp"), onNetwork("n", "10.0.0.1")))
	rec := run(t, api, docker.Config{})
	waitFor(t, time.Second, func() bool { _, ok := rec.get("c1"); return ok })
	if c, _ := rec.get("c1"); c.Name != "billing" {
		t.Fatalf("got %q", c.Name)
	}
}

func TestPublishedPortsAndTraefikDerivation(t *testing.T) {
	t.Parallel()

	labels := with(map[string]string{
		"traefik.http.routers.o.rule": "Host(`api.example.com`)", "traefik.http.routers.o.tls": "true",
	})
	api := newFakeAPI()
	api.set(
		inspect(
			"c1",
			"orders",
			true,
			labels,
			exposing("8000/tcp"),
			onNetwork("n", "192.0.2.1"),
			publishing("8000/tcp", "0.0.0.0", "49153"),
		),
	)
	rec := run(t, api, docker.Config{PreferPublishedPorts: true, TraefikServerURL: true})

	waitFor(t, time.Second, func() bool { _, ok := rec.get("c1"); return ok })
	derived, _ := rec.get("c1")
	if derived.BaseURL != "http://127.0.0.1:49153" || derived.ServerURL != "https://api.example.com" {
		t.Fatalf("got %+v", derived)
	}

	// Without the options the network IP is used and no URL is derived.
	api2 := newFakeAPI()
	api2.set(
		inspect(
			"c1",
			"orders",
			true,
			labels,
			exposing("8000/tcp"),
			onNetwork("n", "192.0.2.1"),
			publishing("8000/tcp", "0.0.0.0", "49153"),
		),
	)
	rec2 := run(t, api2, docker.Config{})
	waitFor(t, time.Second, func() bool { _, ok := rec2.get("c1"); return ok })
	if c, _ := rec2.get("c1"); c.BaseURL != "http://192.0.2.1:8000" || c.ServerURL != "" {
		t.Fatalf("defaults: got %+v", c)
	}
}

func TestEventsDriveStateAndDestroyRemoves(t *testing.T) {
	t.Parallel()

	api := newFakeAPI()
	api.set(inspect("c1", "orders", true, enabled(), exposing("80/tcp"), onNetwork("n", "10.0.0.1")))
	rec := run(t, api, docker.Config{})
	waitFor(t, time.Second, func() bool { c, ok := rec.get("c1"); return ok && c.Running })

	// die -> stopped
	api.set(inspect("c1", "orders", false, enabled(), exposing("80/tcp")))
	api.events <- events.Message{Type: events.ContainerEventType, Action: events.ActionDie, Actor: events.Actor{ID: "c1"}}
	waitFor(t, time.Second, func() bool { c, _ := rec.get("c1"); return !c.Running })

	// health_status carries a suffix on a real daemon
	api.set(
		inspect(
			"c1",
			"orders",
			true,
			enabled(),
			exposing("80/tcp"),
			onNetwork("n", "10.0.0.1"),
			healthy(container.Unhealthy),
		),
	)
	api.events <- events.Message{Type: events.ContainerEventType, Action: "health_status: unhealthy", Actor: events.Actor{ID: "c1"}}
	waitFor(t, time.Second, func() bool { c, _ := rec.get("c1"); return c.Health == "unhealthy" })

	// unrelated events and other types are ignored
	api.events <- events.Message{Type: events.ContainerEventType, Action: events.ActionAttach, Actor: events.Actor{ID: "c1"}}
	api.events <- events.Message{Type: events.NetworkEventType, Action: events.ActionDestroy, Actor: events.Actor{ID: "c1"}}

	api.events <- events.Message{Type: events.ContainerEventType, Action: events.ActionDestroy, Actor: events.Actor{ID: "c1"}}
	waitFor(t, time.Second, func() bool { return len(rec.removedIDs()) == 1 })
	if got := rec.removedIDs(); got[0] != "c1" {
		t.Fatalf("removed %v", got)
	}
}

func TestReconnectsAndResyncsAfterStreamError(t *testing.T) {
	t.Parallel()

	api := newFakeAPI()
	api.set(inspect("c1", "orders", true, enabled(), exposing("80/tcp"), onNetwork("n", "10.0.0.1")))
	rec := run(t, api, docker.Config{})
	waitFor(t, time.Second, func() bool { _, ok := rec.get("c1"); return ok })

	// The daemon restarts: the stream breaks, and while it is down the container is stopped and another appears.
	api.set(inspect("c1", "orders", false, enabled(), exposing("80/tcp")))
	api.set(inspect("c2", "new", true, enabled(), exposing("80/tcp"), onNetwork("n", "10.0.0.2")))
	api.streamErrs <- errors.New("connection reset")

	waitFor(t, 2*time.Second, func() bool { c, ok := rec.get("c2"); return ok && c.Running })
	if c, _ := rec.get("c1"); c.Running {
		t.Fatalf("the resync must notice that c1 stopped: %+v", c)
	}
	if api.sessionCount() < 2 {
		t.Fatalf("expected a reconnect, sessions=%d", api.sessionCount())
	}
}

func TestResyncRetainsOnlyLiveContainers(t *testing.T) {
	t.Parallel()

	api := newFakeAPI()
	api.set(inspect("c1", "a", true, enabled(), exposing("80/tcp"), onNetwork("n", "10.0.0.1")))
	api.set(inspect("c2", "b", true, enabled(), exposing("80/tcp"), onNetwork("n", "10.0.0.2")))
	rec := run(t, api, docker.Config{})
	waitFor(t, time.Second, func() bool { rec.mu.Lock(); defer rec.mu.Unlock(); return len(rec.retain) == 1 })

	rec.mu.Lock()
	defer rec.mu.Unlock()
	if got := rec.retain[0]; len(got) != 2 || !got["c1"] || !got["c2"] {
		t.Fatalf("retain %v", got)
	}
}

func TestListErrorTriggersRetryNotCrash(t *testing.T) {
	t.Parallel()

	api := newFakeAPI()
	api.listErr = errors.New("daemon starting")
	rec := run(t, api, docker.Config{})
	waitFor(t, time.Second, func() bool { return api.sessionCount() >= 2 }) // retried

	api.mu.Lock()
	api.listErr = nil
	api.mu.Unlock()
	api.set(inspect("c1", "orders", true, enabled(), exposing("80/tcp"), onNetwork("n", "10.0.0.1")))
	waitFor(t, 2*time.Second, func() bool { _, ok := rec.get("c1"); return ok })
}

func TestComposeReplicasShareOneNameAndReplicaKey(t *testing.T) {
	t.Parallel()

	api := newFakeAPI()
	for id, n := range map[string]string{"c1": "1", "c2": "2"} {
		api.set(inspect(id, "proj-orders-"+n, true, with(map[string]string{
			"com.docker.compose.container-number": n,
			"com.docker.compose.service":          "orders",
			"com.docker.compose.project":          "proj",
		}), onNetwork("n", "10.0.0."+n), exposing("8000/tcp")))
	}
	api.set(inspect("c3", "solo", true, with(nil), onNetwork("n", "10.0.0.9"), exposing("8000/tcp")))
	rec := run(t, api, docker.Config{})

	waitFor(t, time.Second, func() bool { _, a := rec.get("c1"); _, b := rec.get("c2"); _, c := rec.get("c3"); return a && b && c })
	c1, _ := rec.get("c1")
	c2, _ := rec.get("c2")
	c3, _ := rec.get("c3")
	if c1.Name != "orders" || c2.Name != "orders" || c1.Replica != "proj/orders" || c1.Replica != c2.Replica {
		t.Fatalf("replicas: %+v / %+v", c1, c2)
	}
	if c3.Replica != "" {
		t.Fatalf("a container outside compose is no replica: %+v", c3)
	}
}

func TestSchemeLabelSelectsHTTPS(t *testing.T) {
	t.Parallel()

	api := newFakeAPI()
	api.set(inspect("c1", "secure", true, with(map[string]string{"swagger-aggregator.scheme": "https"}),
		onNetwork("n", "10.0.0.7"), exposing("8443/tcp")))
	rec := run(t, api, docker.Config{})

	waitFor(t, time.Second, func() bool { _, ok := rec.get("c1"); return ok })
	if c, _ := rec.get("c1"); c.BaseURL != "https://10.0.0.7:8443" {
		t.Fatalf("got %+v", c)
	}
}
