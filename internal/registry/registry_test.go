package registry_test

import (
	"net/url"
	"testing"
	"time"

	"github.com/perryprojects/docker-swagger-aggregator/internal/registry"
	"github.com/perryprojects/docker-swagger-aggregator/internal/status"
)

func get(t *testing.T, r *registry.Registry) registry.Service {
	t.Helper()
	for _, s := range r.List() {
		if s.Name == "api" {
			return s
		}
	}
	t.Fatalf("service \"api\" not found in %v", r.List())
	return registry.Service{}
}

func signalled(ch <-chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

func TestLifecycle(t *testing.T) {
	t.Parallel()
	r := registry.New(registry.Config{})
	ch, unsub := r.Subscribe()
	defer unsub()

	// started, no spec yet -> down
	r.Upsert("api", "c1", true, "")
	if s := get(t, r); s.Status != status.Down {
		t.Fatalf("new service must be down, got %s", s.Status)
	}
	if !signalled(ch) {
		t.Fatal("expected notification on insert")
	}

	// spec fetched -> up
	r.SetSpec("c1", []byte(`{"openapi":"3.0.0"}`))
	up := get(t, r)
	if up.Status != status.Up || up.SpecHash == "" || up.LastSeen == nil {
		t.Fatalf("expected up with spec, got %+v", up)
	}
	if !signalled(ch) {
		t.Fatal("expected notification on up")
	}

	// identical spec again (periodic probe): no notification
	r.SetSpec("c1", []byte(`{"openapi":"3.0.0"}`))
	if signalled(ch) {
		t.Fatal("unchanged probe must not notify")
	}

	// stop -> down, spec kept
	r.Upsert("api", "c1", false, "")
	stopped := get(t, r)
	if stopped.Status != status.Down || stopped.SpecHash == "" {
		t.Fatalf("stopped service must be down and keep spec, got %+v", stopped)
	}
	if spec, _, _ := r.Spec("api"); len(spec) == 0 {
		t.Fatal("spec dropped on stop")
	}

	// restart: still down until the probe succeeds
	r.Upsert("api", "c1", true, "")
	if restarted := get(t, r); restarted.Status != status.Down {
		t.Fatalf("restarted service must be down until probed, got %s", restarted.Status)
	}
	r.SetSpec("c1", []byte(`{"openapi":"3.0.0"}`))
	if again := get(t, r); again.Status != status.Up {
		t.Fatal("expected up after probe")
	}

	// unhealthy overrides a good probe
	r.Upsert("api", "c1", true, "unhealthy")
	if sick := get(t, r); sick.Status != status.Down {
		t.Fatal("unhealthy must be down")
	}

	// destroy -> removed
	r.Remove("c1")
	if len(r.List()) != 0 {
		t.Fatal("destroyed service must be removed")
	}
}

func TestProblemMarksDown(t *testing.T) {
	t.Parallel()
	r := registry.New(registry.Config{})
	r.Upsert("api", "c1", true, "")
	r.SetSpec("c1", []byte(`{}`))
	r.SetProblem("c1", "connection refused")
	svc := get(t, r)
	if svc.Status != status.Down || svc.Error != "connection refused" {
		t.Fatalf("got %+v", svc)
	}
}

func TestRecreateReplacesStoppedKeepsSpec(t *testing.T) {
	t.Parallel()
	r := registry.New(registry.Config{})
	r.Upsert("api", "old", true, "")
	r.SetSpec("old", []byte(`{"v":1}`))
	r.Upsert("api", "old", false, "") // die

	r.Upsert("api", "new", true, "") // recreated container
	if got := len(r.List()); got != 1 {
		t.Fatalf("want 1 service, got %d", got)
	}
	if carried := get(t, r); carried.SpecHash == "" {
		t.Fatal("last known spec should carry over to the replacement")
	}

	r.Remove("old") // late destroy of the old container must not affect the new one
	if len(r.List()) != 1 {
		t.Fatal("destroy of replaced container removed its successor")
	}
}

func TestReplicasGetSuffix(t *testing.T) {
	t.Parallel()
	r := registry.New(registry.Config{})
	r.Upsert("api", "aaaaaaaa", true, "")
	r.Upsert("api", "bbbbbbbb", true, "")
	l := r.List()
	if len(l) != 2 || l[0].Name != "api" || l[1].Name != "api-bbbbbb" {
		t.Fatalf("got %+v", l)
	}
}

func TestRetain(t *testing.T) {
	t.Parallel()
	r := registry.New(registry.Config{})
	r.Upsert("a", "c1", true, "")
	r.Upsert("b", "c2", true, "")
	r.Retain(map[string]bool{"c2": true})
	if l := r.List(); len(l) != 1 || l[0].Name != "b" {
		t.Fatalf("got %+v", l)
	}
}

func TestLastSeenOnlyChangeDoesNotNotify(t *testing.T) {
	t.Parallel()
	now := time.Unix(0, 0)
	r := registry.New(registry.Config{Now: func() time.Time { return now }})
	r.Upsert("a", "c1", true, "")
	r.SetSpec("c1", []byte(`{}`))
	ch, unsub := r.Subscribe()
	defer unsub()
	now = now.Add(time.Minute)
	r.SetSpec("c1", []byte(`{}`))
	if signalled(ch) {
		t.Fatal("lastSeen-only update must not notify")
	}
}

func TestRouteMapsPublicURLToContainerOnly(t *testing.T) {
	t.Parallel()

	r := registry.New(registry.Config{})
	r.Upsert("api", "c1", true, "")
	r.SetRoute("c1", registry.Upstream{Base: "http://10.0.0.5:8000", PublicURL: "https://api.example.com"})
	r.Upsert("billing", "c2", true, "")
	r.SetRoute("c2", registry.Upstream{Base: "http://10.0.0.6:9000", PublicURL: "https://api.example.com/billing"})
	r.Upsert("idle", "c3", false, "")
	r.SetRoute("c3", registry.Upstream{Base: "http://10.0.0.7:7000", PublicURL: "https://idle.example.com"})

	tests := []struct {
		target string
		want   string // "" = refused
	}{
		{"https://api.example.com/users?x=1", "http://10.0.0.5:8000/users?x=1"},
		{"https://API.example.com/users", "http://10.0.0.5:8000/users"},
		{"https://api.example.com/billing/invoices", "http://10.0.0.6:9000/invoices"}, // longest prefix wins
		{"https://api.example.com/billingx", "http://10.0.0.5:8000/billingx"},         // not a prefix match
		{"http://api.example.com/users", ""},                                          // scheme differs
		{"https://evil.example.com/users", ""},                                        // unregistered host
		{"https://idle.example.com/x", ""},                                            // stopped container
		{"http://127.0.0.1:8080/x", ""},
	}
	for _, tt := range tests {
		target, err := url.Parse(tt.target)
		if err != nil {
			t.Fatal(err)
		}
		got, ok := r.Route(target)
		switch {
		case tt.want == "" && ok:
			t.Errorf("%s: routed to %s, want refusal", tt.target, got)
		case tt.want != "" && (!ok || got.String() != tt.want):
			t.Errorf("%s: got %v (ok=%v), want %s", tt.target, got, ok, tt.want)
		}
	}
}

func TestMetaChangeNotifies(t *testing.T) {
	t.Parallel()

	r := registry.New(registry.Config{})
	r.Upsert("a", "c1", true, "")
	ch, unsub := r.Subscribe()
	defer unsub()
	r.SetMeta("c1", "payments", 2)
	if !signalled(ch) {
		t.Fatal("group/order change must notify")
	}
	if l := r.List(); l[0].Group != "payments" || l[0].Order != 2 {
		t.Fatalf("got %+v", l[0])
	}
}

func TestRouteKeepsBasePathPrefix(t *testing.T) {
	t.Parallel()

	r := registry.New(registry.Config{})
	r.Upsert("api", "c1", true, "")
	r.SetRoute(
		"c1",
		registry.Upstream{
			Base:      "https://127.0.0.1:6443/api/v1/namespaces/ns/services/api:80/proxy",
			PublicURL: "https://api.example.com/v1",
		},
	)
	target, _ := url.Parse("https://api.example.com/v1/items?x=1")
	got, ok := r.Route(target)
	if !ok || got.String() != "https://127.0.0.1:6443/api/v1/namespaces/ns/services/api:80/proxy/items?x=1" {
		t.Fatalf("got %v (ok=%v)", got, ok)
	}
}

func TestRouteKeepPublicPathForwardsTheWholePath(t *testing.T) {
	t.Parallel()

	r := registry.New(registry.Config{})
	r.Upsert("strip", "c1", true, "")
	r.SetRoute("c1", registry.Upstream{Base: "http://10.0.0.1:80", PublicURL: "https://h.example.com/strip"})
	r.Upsert("keep", "c2", true, "")
	r.SetRoute(
		"c2",
		registry.Upstream{Base: "http://10.0.0.2:80", PublicURL: "https://h.example.com/api", KeepPublicPath: true},
	)

	for in, want := range map[string]string{
		"https://h.example.com/strip/items?x=1": "http://10.0.0.1:80/items?x=1",     // default: prefix replaced by the root
		"https://h.example.com/api/items?x=1":   "http://10.0.0.2:80/api/items?x=1", // kept: the workload serves under /api
		"https://h.example.com/api":             "http://10.0.0.2:80/api",
	} {
		target, _ := url.Parse(in)
		got, ok := r.Route(target)
		if !ok || got.String() != want {
			t.Errorf("%s -> %v (ok=%v), want %s", in, got, ok, want)
		}
	}
}

func TestReplicasAreOneServiceUpWhileAnyReplicaIsUp(t *testing.T) {
	t.Parallel()

	r := registry.New(registry.Config{})
	r.UpsertReplica("api", "c1", "proj/api", true, "")
	r.UpsertReplica("api", "c2", "proj/api", true, "")
	r.SetSpec("c1", []byte(`{"v":1}`))
	r.SetSpec("c2", []byte(`{"v":1}`))

	if l := r.List(); len(l) != 1 || l[0].Name != "api" || l[0].Status != status.Up {
		t.Fatalf("two replicas must be one up service: %+v", l)
	}
	ch, unsub := r.Subscribe()
	defer unsub()

	r.UpsertReplica("api", "c1", "proj/api", false, "") // one replica stops
	if s := get(t, r); s.Status != status.Up || s.SpecHash == "" {
		t.Fatalf("service must stay up with its spec while another replica runs: %+v", s)
	}
	if signalled(ch) {
		t.Fatal("a replica stopping must not change what the UI shows")
	}

	r.UpsertReplica("api", "c2", "proj/api", false, "") // the last one stops
	if s := get(t, r); s.Status != status.Down || s.SpecHash == "" {
		t.Fatalf("all replicas down: down with the last known spec: %+v", s)
	}
	if !signalled(ch) {
		t.Fatal("the service going down must notify")
	}

	r.Remove("c1")
	if len(r.List()) != 1 {
		t.Fatal("removing one replica keeps the service")
	}
	r.Remove("c2")
	if len(r.List()) != 0 {
		t.Fatal("removing the last replica removes the service")
	}
}

func TestSameNameWithoutMatchingReplicaKeyStaysSeparate(t *testing.T) {
	t.Parallel()

	r := registry.New(registry.Config{})
	r.UpsertReplica("api", "c1", "proj-a/api", true, "")
	r.UpsertReplica("api", "c2", "proj-b/api", true, "") // another project: not a replica
	r.Upsert("api", "c3", true, "")                      // no key at all
	if l := r.List(); len(l) != 3 {
		t.Fatalf("unrelated services with one name must not merge: %+v", l)
	}
}
