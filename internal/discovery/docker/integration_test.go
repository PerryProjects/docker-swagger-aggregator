//go:build integration

package docker_test

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/moby/moby/client"
	"go.uber.org/zap"

	"github.com/perryprojects/docker-swagger-aggregator/internal/discovery"
	"github.com/perryprojects/docker-swagger-aggregator/internal/discovery/docker"
	"github.com/perryprojects/docker-swagger-aggregator/internal/fetcher"
	"github.com/perryprojects/docker-swagger-aggregator/internal/registry"
	"github.com/perryprojects/docker-swagger-aggregator/internal/status"
)

// Runs the real discovery stack against the local Docker daemon:
//
//	go test -tags=integration -count=1 -timeout 180s ./internal/discovery -run Integration
//
// It needs the docker CLI (for fixtures) and a daemon, and skips otherwise. To stay invisible to
// any swagger-aggregator instance already running on the daemon, everything uses a unique label prefix
// (so the default `swagger-aggregator.enable=true` is never set), network and container names.

const (
	itImage = "busybox:1.37"
	itSpec  = `{"openapi":"3.0.3","info":{"title":"it","version":"1"},"paths":{}}`
	itWait  = 30 * time.Second
)

func dockerCLI(t *testing.T, args ...string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	out, err := exec.CommandContext(ctx, "docker", args...).
		CombinedOutput()
	if err != nil {
		t.Fatalf("docker %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

func TestIntegrationLifecycleOnRealDaemon(t *testing.T) {
	t.Parallel()

	if out, err := exec.CommandContext(context.Background(), "docker", "version", "--format", "{{.Server.Version}}").
		CombinedOutput(); err != nil {
		t.Skipf("no usable docker daemon: %v (%s)", err, strings.TrimSpace(string(out)))
	}

	id := fmt.Sprintf("%x", time.Now().UnixNano())
	prefix := "swagger-aggregator-it-" + id
	network := prefix
	dockerCLI(t, "network", "create", network)
	var created []string
	t.Cleanup(func() {
		for _, c := range created {
			_ = exec.CommandContext(context.Background(), "docker", "rm", "-f", c).
				Run()
		}
		_ = exec.CommandContext(context.Background(), "docker", "network", "rm", network).Run()
	})

	// run starts a busybox container serving itSpec on :8000 with our unique label prefix.
	run := func(name string, extra ...string) {
		args := []string{
			"run", "-d", "--name", name, "--network", network,
			"--label", prefix + ".enable=true", "--label", prefix + ".port=8000",
			"-p", "127.0.0.1::8000", // random host port; used when container IPs are not routable
			"-e", "SPEC=" + itSpec,
		}
		args = append(args, extra...)
		args = append(args, itImage, "sh", "-c",
			`mkdir -p /www && echo "$SPEC" > /www/openapi.json && touch /tmp/ok && exec httpd -f -p 8000 -h /www`)
		dockerCLI(t, args...)
		created = append(created, name)
	}
	plain := prefix + "-plain"
	hc := prefix + "-hc"
	run(plain)
	run(
		hc,
		"--health-cmd",
		"test -f /tmp/ok",
		"--health-interval",
		"1s",
		"--health-timeout",
		"1s",
		"--health-retries",
		"1",
	)

	cli, err := client.New(client.FromEnv)
	if err != nil {
		t.Fatal(err)
	}
	defer cli.Close()
	log := zap.NewNop()
	reg := registry.New(registry.Config{})
	mgr := discovery.NewManager(discovery.ManagerConfig{
		Store:   reg,
		Fetcher: fetcher.New(fetcher.Config{Client: &http.Client{Timeout: 2 * time.Second}, MaxBytes: 1 << 20}),
		Prepare: func(raw []byte, serverURL, id string) ([]byte, error) {
			return fetcher.Prepare(raw, serverURL, "/p/"+id)
		},
		Log: log, Interval: 500 * time.Millisecond,
	})
	// Docker Desktop (and remote daemons) do not route container IPs to the host.
	viaPublished := !containerIPReachable(t, plain)
	t.Logf("reaching containers via published ports: %v", viaPublished)
	src := docker.New(
		cli,
		docker.Config{Network: network, LabelPrefix: prefix, PreferPublishedPorts: viaPublished},
		log,
	)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go src.Run(ctx, mgr)

	find := func(name string) (registry.Service, bool) {
		for _, s := range reg.List() {
			if s.Name == name {
				return s, true
			}
		}
		return registry.Service{}, false
	}
	is := func(name string, want status.Status) func() bool {
		return func() bool { s, ok := find(name); return ok && s.Status == want }
	}

	// (a) discovered and up, with a spec
	waitFor(t, itWait, is(plain, status.Up))
	if s, _ := find(plain); s.SpecHash == "" {
		t.Fatalf("up without spec: %+v", s)
	}
	waitFor(t, itWait, is(hc, status.Up)) // up only after Docker reports it healthy

	// the unique prefix keeps foreign (e.g. the user's own) containers out
	if n := len(reg.List()); n != 2 {
		t.Fatalf("expected exactly the 2 fixture services, got %+v", reg.List())
	}

	// (b) stop: down, last spec kept, and NOT an error (regression: stopped containers have no network settings)
	dockerCLI(t, "stop", "-t", "1", plain)
	waitFor(t, itWait, is(plain, status.Down))
	if s, _ := find(plain); s.SpecHash == "" || s.Error != "" {
		t.Fatalf("a stopped container keeps its spec and is not an error: %+v", s)
	}

	// (c) start again: up again
	dockerCLI(t, "start", plain)
	waitFor(t, itWait, is(plain, status.Up))

	// (e) health_status events: unhealthy -> down, healthy -> up, with the HTTP probe fine throughout
	dockerCLI(t, "exec", hc, "rm", "/tmp/ok")
	waitFor(t, itWait, is(hc, status.Down))
	dockerCLI(t, "exec", hc, "touch", "/tmp/ok")
	waitFor(t, itWait, is(hc, status.Up))

	// (d) destroy: removed
	dockerCLI(t, "rm", "-f", plain)
	waitFor(t, itWait, func() bool { _, ok := find(plain); return !ok })
	if _, ok := find(hc); !ok {
		t.Fatal("destroying one container must not remove the other")
	}
}

// containerIPReachable reports whether the host can open a TCP connection to the container's IP.
func containerIPReachable(t *testing.T, container string) bool {
	t.Helper()
	ip := dockerCLI(t, "inspect", "-f", "{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}", container)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", net.JoinHostPort(ip, "8000"))
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}
