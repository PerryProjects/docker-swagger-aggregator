// Package registry holds the in-memory state of all discovered services.
package registry

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/perryprojects/docker-swagger-aggregator/internal/status"
)

// shortIDLen is the length of the ID suffix used to disambiguate duplicate names.
const shortIDLen = 6

// Service is the public, JSON-serialisable view of a discovered service.
type Service struct {
	Name     string        `json:"name"`
	Status   status.Status `json:"status"`
	LastSeen *time.Time    `json:"lastSeen,omitempty"` // last successful spec fetch
	SpecHash string        `json:"specHash,omitempty"`
	Error    string        `json:"error,omitempty"` // last fetch/discovery problem
	Group    string        `json:"group,omitempty"` // optional UI grouping label
	Order    int           `json:"order,omitempty"` // optional sort key inside a group

	// What the current spec says about the service (set together with SpecHash).
	Title     string `json:"title,omitempty"`     // info.title
	Version   string `json:"version,omitempty"`   // info.version
	OpenAPI   string `json:"openapi,omitempty"`   // "3.1.0", "2.0", ...
	Endpoints int    `json:"endpoints,omitempty"` // number of operations
	// Where and how it runs.
	Source     string `json:"source,omitempty"`     // "docker" or "kubernetes"
	Health     string `json:"health,omitempty"`     // Docker health status (healthy, unhealthy, starting); empty without a health check
	Replicas   int    `json:"replicas,omitempty"`   // workloads behind this service (more than one when scaled)
	ReplicasUp int    `json:"replicasUp,omitempty"` // how many of them are up
}

// Upstream says where a workload can be reached and how its public URL maps onto it.
type Upstream struct {
	// Base is the address the proxies forward to, e.g. http://10.0.0.5:8080 (may carry a path prefix).
	Base string
	// PublicURL is the server-url: the public address that maps to Base ("" if there is none).
	PublicURL string
	// KeepPublicPath forwards the public URL's path unchanged instead of replacing it by Base's root. Use
	// it when the workload itself serves under that path (a proxy that does not strip the prefix).
	KeepPublicPath bool
}

type entry struct {
	svc       Service
	id        string
	key       string // replica key: entries of one name with the same non-empty key are replicas of one service
	in        status.Inputs
	spec      []byte
	base      string   // Upstream.Base
	publicURL *url.URL // Upstream.PublicURL, parsed
	keepPath  bool     // Upstream.KeepPublicPath
}

// Config holds the injectable dependencies; the zero value is usable.
type Config struct {
	// Now returns the current time. Defaults to time.Now.
	Now func() time.Time
}

// Registry is safe for concurrent use. Workloads are identified by an opaque ID
// (a container ID, a Kubernetes UID, ...); a service is addressed by its unique name and
// consists of one workload or, for replicas, several (see UpsertReplica).
type Registry struct {
	now     func() time.Time
	version atomic.Uint64

	mu      sync.RWMutex
	entries map[string]*entry   // by ID
	byName  map[string][]*entry // replicas of one service, in arrival order
	subs    map[chan struct{}]struct{}
}

// New creates an empty registry.
func New(cfg Config) *Registry {
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	return &Registry{
		now:     now,
		entries: map[string]*entry{},
		byName:  map[string][]*entry{},
		subs:    map[chan struct{}]struct{}{},
	}
}

// Upsert records a workload's state. A stopped workload stays in the registry
// (with its last known spec) and is marked down.
//
// Name collisions: a running workload owns its name. A new one with the name of
// a stopped one replaces it (e.g. compose recreate) and inherits the last known
// spec; with a running one it gets a "-<id>" suffix.
func (r *Registry) Upsert(name, id string, running bool, health string) {
	r.UpsertReplica(name, id, "", running, health)
}

// UpsertReplica is Upsert for workloads that may be replicas (e.g. compose `--scale`): a new workload
// whose name is owned by a running one with the same non-empty replicaKey joins it. The replicas show as
// one service that is up while any replica is up.
func (r *Registry) UpsertReplica(name, id, replicaKey string, running bool, health string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	e := r.entries[id]
	if e == nil {
		e = &entry{id: id, key: replicaKey}
		name = r.claimName(e, name)
		e.svc.Name = name
		r.entries[id] = e
		r.byName[name] = append(r.byName[name], e)
	}
	r.mutate(e, func() {
		e.in.Running = running
		e.in.Health = health
		e.svc.Health = health
		if health == "none" {
			e.svc.Health = ""
		}
		if !running {
			e.in.ProbeOK = false
		}
	})
}

// claimName decides under which name a new entry is registered (see Upsert/UpsertReplica).
// Callers hold r.mu for writing.
func (r *Registry) claimName(e *entry, name string) string {
	group := r.byName[name]
	if len(group) == 0 {
		return name
	}
	if slices.ContainsFunc(group, func(o *entry) bool { return o.in.Running }) {
		if e.key != "" && group[0].key == e.key {
			return name // a replica of the running service
		}
		return name + "-" + shortID(e.id)
	}
	// Only stopped workloads own the name: the new one replaces them and inherits the last known spec.
	old := r.representative(group)
	e.spec, e.svc.SpecHash, e.svc.LastSeen = old.spec, old.svc.SpecHash, old.svc.LastSeen
	for _, o := range slices.Clone(group) {
		r.drop(o)
	}
	return name
}

// SetRoute records where the workload can be reached and, optionally, the public
// server-url that maps to it (used by the proxies).
func (r *Registry) SetRoute(id string, up Upstream) {
	r.mu.Lock()
	defer r.mu.Unlock()
	e := r.entries[id]
	if e == nil {
		return
	}
	e.base, e.keepPath = up.Base, up.KeepPublicPath
	e.publicURL = nil
	if u, err := url.Parse(up.PublicURL); err == nil && u.Host != "" {
		e.publicURL = u
	}
}

// SetSource records which source found the workload ("docker", "kubernetes").
func (r *Registry) SetSource(id, source string) {
	r.update(id, func(e *entry) { e.svc.Source = source })
}

// SetMeta records the optional group/order metadata.
func (r *Registry) SetMeta(id, group string, order int) {
	r.update(id, func(e *entry) { e.svc.Group, e.svc.Order = group, order })
}

// Route maps a public URL (below some running service's server-url) to the
// URL of the workload behind it. It only ever returns URLs under a registered
// base, so it cannot be used to reach arbitrary hosts. The longest matching
// server-url path wins; that path is replaced by the workload root.
func (r *Registry) Route(target *url.URL) (*url.URL, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var best *entry
	bestLen := -1
	for _, e := range r.entries {
		p := e.publicURL
		if p == nil || e.base == "" || !e.in.Running ||
			!strings.EqualFold(p.Scheme, target.Scheme) || !strings.EqualFold(p.Host, target.Host) {
			continue
		}
		prefix := strings.TrimRight(p.Path, "/")
		if (target.Path == prefix || strings.HasPrefix(target.Path, prefix+"/") || prefix == "") &&
			len(prefix) > bestLen {
			best, bestLen = e, len(prefix)
		}
	}
	if best == nil {
		return nil, false
	}
	up, err := url.Parse(best.base)
	if err != nil {
		return nil, false
	}
	rest := strings.TrimPrefix(target.Path, "/") // KeepPublicPath: the whole public path
	if !best.keepPath {
		rest = strings.TrimPrefix(strings.TrimPrefix(target.Path, strings.TrimRight(best.publicURL.Path, "/")), "/")
	}
	up.Path = strings.TrimRight(up.Path, "/") + "/" + rest // keep a base path prefix
	up.RawQuery = target.RawQuery
	return up, true
}

// Base returns the reachable base URL of a workload.
func (r *Registry) Base(id string) (string, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	e := r.entries[id]
	if e == nil || e.base == "" {
		return "", false
	}
	return e.base, true
}

// SetSpec stores a freshly fetched spec and marks the probe successful.
func (r *Registry) SetSpec(id string, spec []byte) {
	sum := sha256.Sum256(spec)
	r.update(id, func(e *entry) {
		now := r.now()
		e.spec, e.svc.SpecHash, e.svc.LastSeen = spec, hex.EncodeToString(sum[:8]), &now
		e.svc.Title, e.svc.Version, e.svc.OpenAPI, e.svc.Endpoints = specSummary(spec)
		e.svc.Error = ""
		e.in.ProbeOK = true
	})
}

// Touch marks a probe as successful without changing the stored spec.
func (r *Registry) Touch(id string) {
	r.update(id, func(e *entry) {
		now := r.now()
		e.svc.LastSeen = &now
		e.svc.Error = ""
		e.in.ProbeOK = true
	})
}

// SetProblem records a failed fetch (or other problem) and marks the probe failed.
func (r *Registry) SetProblem(id, msg string) {
	r.update(id, func(e *entry) {
		e.svc.Error = msg
		e.in.ProbeOK = false
	})
}

// Remove deletes the service of a destroyed workload. Unknown IDs are ignored,
// so the destroy event of a replaced workload cannot remove its successor.
func (r *Registry) Remove(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if e := r.entries[id]; e != nil {
		r.drop(e)
		r.notify()
	}
}

// Retain drops every service whose ID is not in ids (used after a resync with
// the source, to catch removals missed while disconnected).
func (r *Registry) Retain(ids map[string]bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	changed := false
	for id, e := range r.entries {
		if !ids[id] {
			r.drop(e)
			changed = true
		}
	}
	if changed {
		r.notify()
	}
}

// List returns all services sorted by name.
func (r *Registry) List() []Service {
	r.mu.RLock()
	out := make([]Service, 0, len(r.byName))
	for name := range r.byName {
		if svc, ok := r.view(name); ok {
			out = append(out, svc)
		}
	}
	r.mu.RUnlock()
	slices.SortFunc(out, func(a, b Service) int { return strings.Compare(a.Name, b.Name) })
	return out
}

// Spec returns the last known spec (and its hash) for a service name.
func (r *Registry) Spec(name string) ([]byte, string, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	group := r.byName[name]
	if len(group) == 0 {
		return nil, "", false
	}
	rep := r.representative(group)
	return rep.spec, rep.svc.SpecHash, true
}

// representative is the replica a service is shown (and documented) by: one that is up and has a spec, else one
// that is up, else one with a spec. Ties are broken by a fixed order (the lowest ID), not by arrival order: several
// API instances watch the same workloads, see them in different orders and must still agree, or the spec hash
// (which contains the replica's proxy path) would differ between them. Callers hold r.mu.
func (r *Registry) representative(group []*entry) *entry {
	best := group[0]
	for _, e := range group[1:] {
		if better(e, best) {
			best = e
		}
	}
	return best
}

func rank(e *entry) int {
	switch {
	case e.svc.Status == status.Up && e.spec != nil:
		return 3
	case e.svc.Status == status.Up:
		return 2
	case e.spec != nil:
		return 1
	}
	return 0
}

// better orders replicas: higher rank first; among down replicas the one seen last; then the lowest ID.
func better(a, b *entry) bool {
	if ra, rb := rank(a), rank(b); ra != rb {
		return ra > rb
	}
	if rank(a) < 2 {
		if seenAfter(a, b) {
			return true
		}
		if seenAfter(b, a) {
			return false
		}
	}
	return a.id < b.id
}

func seenAfter(a, b *entry) bool {
	return a.svc.LastSeen != nil && (b.svc.LastSeen == nil || a.svc.LastSeen.After(*b.svc.LastSeen))
}

// view is the public Service of a name: its representative replica. Callers hold r.mu.
func (r *Registry) view(name string) (Service, bool) {
	group := r.byName[name]
	if len(group) == 0 {
		return Service{}, false
	}
	svc := r.representative(group).svc
	svc.Replicas = len(group)
	for _, e := range group {
		if e.svc.Status == status.Up {
			svc.ReplicasUp++
		}
	}
	return svc, true
}

// Version increases whenever the visible service list changes. Callers can use
// it to cache anything derived from List.
func (r *Registry) Version() uint64 { return r.version.Load() }

// Subscribe returns a channel that receives a (coalesced) signal whenever the
// visible service list changes, and a function to unsubscribe.
func (r *Registry) Subscribe() (<-chan struct{}, func()) {
	ch := make(chan struct{}, 1)
	r.mu.Lock()
	r.subs[ch] = struct{}{}
	r.mu.Unlock()
	return ch, func() {
		r.mu.Lock()
		delete(r.subs, ch)
		r.mu.Unlock()
	}
}

// SubscriberCount reports the number of active subscriptions (leak checks).
func (r *Registry) SubscriberCount() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.subs)
}

func (r *Registry) update(id string, f func(*entry)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if e := r.entries[id]; e != nil {
		r.mutate(e, func() { f(e) })
	}
}

// drop removes e from both indexes. Callers hold r.mu for writing.
func (r *Registry) drop(e *entry) {
	delete(r.entries, e.id)
	group := slices.DeleteFunc(slices.Clone(r.byName[e.svc.Name]), func(o *entry) bool { return o == e })
	if len(group) == 0 {
		delete(r.byName, e.svc.Name)
		return
	}
	r.byName[e.svc.Name] = group
}

// mutate applies f, recomputes the status and notifies subscribers if the
// visible state of the service changed. LastSeen alone does not count as a change: it only
// moves on successful probes, which would otherwise emit an event per probe.
// Callers hold r.mu for writing.
func (r *Registry) mutate(e *entry, f func()) {
	before, hadBefore := r.view(e.svc.Name)
	f()
	e.svc.Status = status.Compute(e.in)
	after, _ := r.view(e.svc.Name)
	if !hadBefore || before.Name != after.Name || before.Status != after.Status ||
		before.SpecHash != after.SpecHash || before.Error != after.Error ||
		before.Group != after.Group || before.Order != after.Order ||
		before.Health != after.Health || before.Source != after.Source ||
		before.Replicas != after.Replicas || before.ReplicasUp != after.ReplicasUp {
		r.notify()
	}
}

func (r *Registry) notify() {
	r.version.Add(1)
	for ch := range r.subs {
		select {
		case ch <- struct{}{}:
		default: // already pending
		}
	}
}

func shortID(id string) string {
	if len(id) > shortIDLen {
		return id[:shortIDLen]
	}
	return id
}

var operationMethods = []string{"get", "put", "post", "delete", "options", "head", "patch", "trace"}

// specSummary reads what the status page shows of a (canonical JSON) spec: title, version, the OpenAPI/Swagger
// version and the number of operations. It never fails: a spec that Prepare accepted can still have odd values.
func specSummary(spec []byte) (title, version, openapi string, endpoints int) {
	var doc struct {
		OpenAPI string `json:"openapi"`
		Swagger string `json:"swagger"`
		Info    struct {
			Title   any `json:"title"`
			Version any `json:"version"`
		} `json:"info"`
		Paths map[string]json.RawMessage `json:"paths"`
	}
	if json.Unmarshal(spec, &doc) != nil {
		return "", "", "", 0
	}
	openapi = doc.OpenAPI
	if openapi == "" {
		openapi = doc.Swagger
	}
	for _, item := range doc.Paths {
		var ops map[string]json.RawMessage
		if json.Unmarshal(item, &ops) != nil {
			continue
		}
		for _, m := range operationMethods {
			if _, ok := ops[m]; ok {
				endpoints++
			}
		}
	}
	return text(doc.Info.Title), text(doc.Info.Version), openapi, endpoints
}

// text turns a JSON string or number (a version written as 1.0 or 2) into a string.
func text(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	}
	return ""
}
