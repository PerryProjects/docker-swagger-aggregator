package docker_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/moby/moby/client"

	"github.com/perryprojects/docker-swagger-aggregator/internal/discovery"
	"github.com/perryprojects/docker-swagger-aggregator/internal/discovery/docker"
	"github.com/perryprojects/docker-swagger-aggregator/internal/fetcher"
	"github.com/perryprojects/docker-swagger-aggregator/internal/registry"
	"github.com/perryprojects/docker-swagger-aggregator/internal/status"
)

// fakeDaemon speaks just enough of the Docker Engine HTTP API (ping, list,
// inspect, events) to drive the source through the REAL client. It proves the
// JSON decoding and the stream handling, not that a real daemon sends exactly
// these payloads - the integration test (build tag) covers that.
type fakeDaemon struct {
	mu      sync.Mutex
	running bool
	port    string
	events  chan string // JSON lines to push on the current /events stream
	drop    chan struct{}
}

var versionPrefix = regexp.MustCompile(`^/v[0-9.]+`)

func (f *fakeDaemon) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	running := f.running
	f.mu.Unlock()
	switch versionPrefix.ReplaceAllString(r.URL.Path, "") {
	case "/_ping":
		w.Header().Set("Api-Version", "1.50")
	case "/containers/json":
		fmt.Fprint(w, `[{"Id":"c1"}]`)
	case "/containers/c1/json":
		// A stopped container has no network settings, like on a real daemon.
		networks := `{}`
		if running {
			networks = `{"demo":{"IPAddress":"127.0.0.1"}}`
		}
		fmt.Fprintf(w, `{"Id":"c1","Name":"/api","State":{"Running":%t},
			"Config":{"Labels":{"swagger-aggregator.enable":"true","swagger-aggregator.port":%q}},
			"NetworkSettings":{"Networks":%s,"Ports":{}}}`, running, f.port, networks)
	case "/events":
		w.Header().Set("Content-Type", "application/x-ndjson")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		flusher.Flush()
		for {
			select {
			case line := <-f.events:
				fmt.Fprintln(w, line)
				flusher.Flush()
			case <-f.drop:
				return // simulate a daemon restart: the stream ends
			case <-r.Context().Done():
				return
			}
		}
	default:
		http.NotFound(w, r)
	}
}

func TestRealClientAgainstFakeDaemon(t *testing.T) {
	t.Parallel()

	app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"openapi":"3.0.0","info":{"title":"t","version":"1"},"paths":{}}`))
	}))
	defer app.Close()
	u, _ := url.Parse(app.URL)

	fd := &fakeDaemon{running: true, port: u.Port(), events: make(chan string, 8), drop: make(chan struct{})}
	daemon := httptest.NewServer(fd)
	defer daemon.Close()
	cli, err := client.New(client.WithHost("tcp://" + strings.TrimPrefix(daemon.URL, "http://")))
	if err != nil {
		t.Fatal(err)
	}

	reg := registry.New(registry.Config{})
	mgr := discovery.NewManager(discovery.ManagerConfig{
		Store:   reg,
		Fetcher: fetcher.New(fetcher.Config{Client: &http.Client{Timeout: time.Second}, MaxBytes: 1 << 20}),
		Prepare: func(raw []byte, serverURL, id string) ([]byte, error) {
			return fetcher.Prepare(raw, serverURL, "/p/"+id)
		},
		Log: discardLog(), Interval: 100 * time.Millisecond,
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go docker.New(cli, docker.Config{Network: "demo", ReconnectBackoff: 20 * time.Millisecond}, discardLog()).
		Run(ctx, mgr)

	state := func() (string, status.Status) {
		l := reg.List()
		if len(l) != 1 {
			return "", ""
		}
		return l[0].Name, l[0].Status
	}
	waitFor(t, 3*time.Second, func() bool { n, s := state(); return n == "api" && s == status.Up })

	// The daemon restarts and the container stopped in the meantime: the resync after
	// reconnecting must notice, although no die event was ever delivered.
	fd.mu.Lock()
	fd.running = false
	fd.mu.Unlock()
	fd.drop <- struct{}{}
	waitFor(t, 5*time.Second, func() bool { _, s := state(); return s == status.Down })
	if l := reg.List(); l[0].SpecHash == "" || l[0].Error != "" {
		t.Fatalf("a stopped container keeps its spec and is not an error: %+v", l[0])
	}

	// start event on the new stream brings it back
	fd.mu.Lock()
	fd.running = true
	fd.mu.Unlock()
	ev, _ := json.Marshal(map[string]any{"Type": "container", "Action": "start", "Actor": map[string]any{"ID": "c1"}})
	fd.events <- string(ev)
	waitFor(t, 3*time.Second, func() bool { _, s := state(); return s == status.Up })

	ev, _ = json.Marshal(map[string]any{"Type": "container", "Action": "destroy", "Actor": map[string]any{"ID": "c1"}})
	fd.events <- string(ev)
	waitFor(t, 3*time.Second, func() bool { return len(reg.List()) == 0 })
}
