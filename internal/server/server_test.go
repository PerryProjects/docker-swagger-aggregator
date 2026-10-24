package server_test

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/fstest"
	"time"

	"go.uber.org/zap"

	"github.com/perryprojects/docker-swagger-aggregator/internal/registry"
	"github.com/perryprojects/docker-swagger-aggregator/internal/server"
)

// readEvent returns the next SSE frame (lines up to a blank line) or fails on timeout.
func readEvent(t *testing.T, sc *bufio.Scanner) string {
	const within = time.Second
	t.Helper()
	got := make(chan string, 1)
	go func() {
		var b strings.Builder
		for sc.Scan() {
			if sc.Text() == "" {
				got <- b.String()
				return
			}
			b.WriteString(sc.Text() + "\n")
		}
		got <- "EOF"
	}()
	select {
	case s := <-got:
		return s
	case <-time.After(within):
		t.Fatal("no SSE frame within", within, "- response is buffered or stalled")
		return ""
	}
}

// Runs against a real listener (not a ResponseRecorder) so buffering shows up:
// each frame must arrive while the request is still open.
func TestSSEStreamsUnbuffered(t *testing.T) {
	t.Parallel()
	reg := registry.New(registry.Config{})
	mux := http.NewServeMux()
	mux.Handle("GET /api/events", newAPI(reg, 50*time.Millisecond))
	ts := httptest.NewServer(mux)
	defer ts.Close()

	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL+"/api/events", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("content-type %q", ct)
	}
	if cc := resp.Header.Get("Cache-Control"); cc != "no-cache" {
		t.Fatalf("cache-control %q", cc)
	}
	sc := bufio.NewScanner(resp.Body)

	// 1. initial snapshot (empty list)
	if f := readEvent(t, sc); !strings.Contains(f, "event: services") || !strings.Contains(f, "data: []") {
		t.Fatalf("initial frame: %q", f)
	}
	// 2. heartbeat comment arrives without any registry change
	if f := readEvent(t, sc); !strings.HasPrefix(f, ": heartbeat") {
		t.Fatalf("expected heartbeat, got %q", f)
	}
	// 3. a registry change is pushed immediately
	reg.Upsert("api", "c1", true, "")
	for {
		f := readEvent(t, sc)
		if strings.HasPrefix(f, ": heartbeat") {
			continue
		}
		var list []registry.Service
		if jsonErr := json.Unmarshal(
			[]byte(strings.TrimPrefix(strings.Split(f, "\n")[1], "data: ")),
			&list,
		); jsonErr != nil {
			t.Fatal(jsonErr, f)
		}
		if len(list) != 1 || list[0].Name != "api" {
			t.Fatalf("got %+v", list)
		}
		break
	}
	// 4. client disconnect releases the subscription
	cancel()
	deadline := time.Now().Add(time.Second)
	for reg.SubscriberCount() != 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if n := reg.SubscriberCount(); n != 0 {
		t.Fatalf("%d subscribers leaked after disconnect", n)
	}
}

func TestSpecEndpoint(t *testing.T) {
	t.Parallel()
	reg := registry.New(registry.Config{})
	reg.Upsert("api", "c1", true, "")
	h := newAPI(reg, 0)

	get := func(path, inm string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		if inm != "" {
			req.Header.Set("If-None-Match", inm)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	if c := get("/api/services/nope/spec", "").Code; c != 404 {
		t.Fatalf("unknown service: %d", c)
	}
	if c := get("/api/services/api/spec", "").Code; c != 404 {
		t.Fatalf("no spec yet: %d", c)
	}
	reg.SetSpec("c1", []byte(`{"openapi":"3.0.0"}`))
	specRec := get("/api/services/api/spec", "")
	if specRec.Code != http.StatusOK || specRec.Body.String() != `{"openapi":"3.0.0"}` {
		t.Fatalf("%d %s", specRec.Code, specRec.Body)
	}
	if c := get("/api/services/api/spec", specRec.Header().Get("ETag")).Code; c != http.StatusNotModified {
		t.Fatalf("etag revalidation: %d", c)
	}
	if health := get("/healthz", ""); health.Code != http.StatusOK {
		t.Fatalf("healthz %d", health.Code)
	}
}

func TestProxyForwardsToRegisteredContainerOnly(t *testing.T) {
	t.Parallel()
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "%s %s?%s cookie=%q origin=%q", r.Method, r.URL.Path, r.URL.RawQuery,
			r.Header.Get("Cookie"), r.Header.Get("Origin"))
	}))
	defer backend.Close()

	reg := registry.New(registry.Config{})
	reg.Upsert("api", "c1", true, "")
	reg.SetRoute("c1", registry.Upstream{Base: backend.URL})
	h := newAPI(reg, 0)

	req := httptest.NewRequest(http.MethodPost, "/api/proxy/c1/v1/items?x=1", nil)
	req.Header.Set("Cookie", "session=secret")
	req.Header.Set("Origin", "https://docs.example.com")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if want := `POST /v1/items?x=1 cookie="" origin=""`; rec.Body.String() != want {
		t.Fatalf("got %q, want %q", rec.Body, want)
	}

	// IDs not in the registry (e.g. an attacker-chosen host) are refused.
	for _, id := range []string{"unknown", "127.0.0.1:9", "evil.example.com"} {
		denied := httptest.NewRecorder()
		h.ServeHTTP(denied, httptest.NewRequest(http.MethodGet, "/api/proxy/"+id+"/x", nil))
		if denied.Code != http.StatusNotFound {
			t.Errorf("id %q: status %d, want 404", id, denied.Code)
		}
	}

	// unreachable upstream -> 502, not a hang or panic
	reg.SetRoute("c1", registry.Upstream{Base: "http://127.0.0.1:1"})
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/proxy/c1/x", nil))
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status %d, want 502", rec.Code)
	}
}

// The web server must forward /api and keep SSE unbuffered end to end.
func TestWebServerServesUIAndStreamsSSEThroughProxy(t *testing.T) {
	t.Parallel()
	reg := registry.New(registry.Config{})
	apiMux := http.NewServeMux()
	apiMux.Handle("GET /api/events", newAPI(reg, 50*time.Millisecond))
	api := httptest.NewServer(apiMux)
	defer api.Close()
	apiURL, _ := url.Parse(api.URL)

	ui := fstest.MapFS{"index.html": {Data: []byte("INDEX")}, "assets/a.js": {Data: []byte("JS")}}
	web := httptest.NewServer(server.NewWebHandler(server.WebConfig{UI: ui, API: apiURL}))
	defer web.Close()

	for path, want := range map[string]string{"/": "INDEX", "/assets/a.js": "JS", "/some/route": "INDEX"} {
		resp, err := http.Get(web.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if string(body) != want {
			t.Errorf("%s: got %q want %q", path, body, want)
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, web.URL+"/api/events", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	sc := bufio.NewScanner(resp.Body)
	if f := readEvent(t, sc); !strings.Contains(f, "event: services") {
		t.Fatalf("initial frame via proxy: %q", f)
	}
	if f := readEvent(t, sc); !strings.HasPrefix(f, ": heartbeat") {
		t.Fatalf("heartbeat must pass through the proxy unbuffered, got %q", f)
	}
}

func TestScalarProxyAllowlistAndLimits(t *testing.T) {
	t.Parallel()

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		fmt.Fprintf(w, "%s %s?%s len=%d", r.Method, r.URL.Path, r.URL.RawQuery, len(body))
	}))
	defer backend.Close()

	reg := registry.New(registry.Config{})
	reg.Upsert("api", "c1", true, "")
	reg.SetRoute("c1", registry.Upstream{Base: backend.URL, PublicURL: "https://api.example.com/v1"})
	h := newAPI(reg, 0)

	do := func(method, scalarURL string, body io.Reader) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(method, "/api/scalar-proxy?scalar_url="+url.QueryEscape(scalarURL), body))
		return rec
	}

	rec := do(http.MethodPost, "https://api.example.com/v1/items?a=b", strings.NewReader("hello"))
	if want := "POST /items?a=b len=5"; rec.Body.String() != want {
		t.Fatalf("got %q, want %q", rec.Body, want)
	}
	// Scalar resolves our relative /api/proxy/<id> servers to absolute URLs and sends them here too.
	viaSelf := do(http.MethodGet, "http://localhost:4173/api/proxy/c1/self?z=1", nil)
	if want := "GET /self?z=1 len=0"; viaSelf.Body.String() != want {
		t.Fatalf("own proxy path via scalar-proxy: got %q, want %q", viaSelf.Body, want)
	}
	if c := do(http.MethodGet, "http://localhost:4173/api/proxy/unknown/x", nil).Code; c != http.StatusNotFound {
		t.Errorf("unknown id via scalar-proxy: status %d, want 404", c)
	}
	for _, evil := range []string{"https://evil.example.com/x", "http://169.254.169.254/latest/meta-data", "https://api.example.com.evil.io/v1/x"} {
		if c := do(http.MethodGet, evil, nil).Code; c != http.StatusNotFound {
			t.Errorf("%s: status %d, want 404", evil, c)
		}
	}
	for _, bad := range []string{"", "ftp://api.example.com/v1", "not a url", "/relative"} {
		if c := do(http.MethodGet, bad, nil).Code; c != http.StatusBadRequest {
			t.Errorf("%q: status %d, want 400", bad, c)
		}
	}

	big := strings.NewReader(strings.Repeat("x", 11<<20))
	if c := do(http.MethodPost, "https://api.example.com/v1/items", big).Code; c == http.StatusOK {
		t.Errorf("oversized body must not be forwarded successfully, got %d", c)
	}
}

func TestWebHeadersAndCaching(t *testing.T) {
	t.Parallel()

	ui := fstest.MapFS{"index.html": {Data: []byte("I")}, "assets/a-123.js": {Data: []byte("J")}}
	apiURL, _ := url.Parse("http://127.0.0.1:1")
	h := server.NewWebHandler(server.WebConfig{UI: ui, API: apiURL})
	get := func(path string) http.Header {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		return rec.Header()
	}
	if cc := get("/assets/a-123.js").Get("Cache-Control"); !strings.Contains(cc, "immutable") {
		t.Errorf("hashed asset cache-control %q", cc)
	}
	for _, p := range []string{"/", "/some/route"} {
		if cc := get(p).Get("Cache-Control"); cc != "no-cache" {
			t.Errorf("%s cache-control %q, want no-cache", p, cc)
		}
	}
	hdr := get("/")
	if hdr.Get("X-Content-Type-Options") != "nosniff" || hdr.Get("X-Frame-Options") != "DENY" {
		t.Errorf("missing security headers: %v", hdr)
	}
}

func TestProbe(t *testing.T) {
	t.Parallel()

	ok := httptest.NewServer(newAPI(registry.New(registry.Config{}), 0))
	defer ok.Close()
	if err := server.Probe(
		context.Background(),
		http.DefaultClient,
		strings.TrimPrefix(ok.URL, "http://"),
	); err != nil {
		t.Fatalf("healthy server: %v", err)
	}
	if err := server.Probe(context.Background(), http.DefaultClient, "127.0.0.1:1"); err == nil {
		t.Fatal("closed port must fail")
	}
	broken := httptest.NewServer(http.NotFoundHandler())
	defer broken.Close()
	if err := server.Probe(
		context.Background(),
		http.DefaultClient,
		strings.TrimPrefix(broken.URL, "http://"),
	); err == nil {
		t.Fatal("non-200 must fail")
	}
}

// newAPI builds the API handler the way production does, with a fast heartbeat if asked.
func newAPI(reg *registry.Registry, heartbeat time.Duration) http.Handler {
	return server.New(server.Config{
		Store: reg, Log: zap.NewNop(), Heartbeat: heartbeat,
	}).Handler()
}

// countingStore is a fake Store (the point of the consumer-side interface): it counts List calls
// and lets the test trigger changes.
type countingStore struct {
	mu      sync.Mutex
	subs    []chan struct{}
	version atomic.Uint64
	lists   atomic.Int32
}

func (s *countingStore) List() []registry.Service {
	s.lists.Add(1)
	return []registry.Service{{Name: "api", Status: "up"}}
}
func (*countingStore) Spec(string) ([]byte, string, bool) { return nil, "", false }
func (*countingStore) Base(string) (string, bool)         { return "", false }
func (*countingStore) Route(*url.URL) (*url.URL, bool)    { return nil, false }
func (s *countingStore) Version() uint64                  { return s.version.Load() }
func (s *countingStore) Subscribe() (<-chan struct{}, func()) {
	ch := make(chan struct{}, 1)
	s.mu.Lock()
	s.subs = append(s.subs, ch)
	s.mu.Unlock()
	return ch, func() {}
}

func (s *countingStore) change() {
	s.version.Add(1)
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, ch := range s.subs {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

func TestSSEClientsShareOneSnapshotPerChange(t *testing.T) {
	t.Parallel()

	store := &countingStore{}
	ts := httptest.NewServer(
		server.New(server.Config{Store: store, Log: zap.NewNop(), Heartbeat: time.Hour}).Handler(),
	)
	defer ts.Close()

	const clients = 25
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	scanners := make([]*bufio.Scanner, clients)
	for i := range clients {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL+"/api/events", nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		scanners[i] = bufio.NewScanner(resp.Body)
		if f := readEvent(t, scanners[i]); !strings.Contains(f, `"name":"api"`) {
			t.Fatalf("client %d initial frame: %q", i, f)
		}
	}
	store.change()
	for i, sc := range scanners {
		if f := readEvent(t, sc); !strings.Contains(f, "event: services") {
			t.Fatalf("client %d did not get the update: %q", i, f)
		}
	}
	// One marshal for the initial version and one for the change, not one per client.
	if n := store.lists.Load(); n > 2 {
		t.Fatalf("List called %d times for %d clients, want at most 2", n, clients)
	}
}

func TestProxyByIDKeepsBasePathPrefix(t *testing.T) {
	t.Parallel()

	var gotPath string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		fmt.Fprint(w, "ok")
	}))
	defer backend.Close()
	reg := registry.New(registry.Config{})
	reg.Upsert("api", "c1", true, "")
	reg.SetRoute("c1", registry.Upstream{Base: backend.URL + "/api/v1/namespaces/ns/services/api:80/proxy"})
	rec := httptest.NewRecorder()
	newAPI(reg, 0).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/proxy/c1/orders", nil))
	if want := "/api/v1/namespaces/ns/services/api:80/proxy/orders"; gotPath != want {
		t.Fatalf("upstream path %q, want %q", gotPath, want)
	}
}

func TestWebBasePathIsStrippedWhenPresent(t *testing.T) {
	t.Parallel()

	ui := fstest.MapFS{"index.html": {Data: []byte("INDEX")}, "assets/a.js": {Data: []byte("JS")}}
	apiURL, _ := url.Parse("http://127.0.0.1:1")
	h := server.NewWebHandler(server.WebConfig{UI: ui, API: apiURL, BasePath: "/docs"})
	for path, want := range map[string]string{
		"/docs/assets/a.js": "JS", "/assets/a.js": "JS", // with and without the prefix (proxy may strip it)
		"/docs/status": "INDEX", "/docs": "INDEX", "/docs/": "INDEX",
	} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if got := rec.Body.String(); got != want {
			t.Errorf("%s: body %q, want %q", path, got, want)
		}
	}
	if got := server.ProxyPath("/docs", "c1"); got != "/docs/api/proxy/c1" {
		t.Errorf("ProxyPath = %q", got)
	}
}
