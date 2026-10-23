//nolint:testpackage // benchmarks the unexported snapshotter
package server

import (
	"encoding/json"
	"fmt"
	"net/url"
	"testing"

	"github.com/perryprojects/docker-swagger-aggregator/internal/registry"
)

type benchStore struct{ services []registry.Service }

func (s benchStore) List() []registry.Service { return s.services }
func (benchStore) Version() uint64            { return 1 }

// storeAdapter fills in the parts of Store the snapshotter never calls.
type storeAdapter struct{ benchStore }

func (storeAdapter) Spec(string) ([]byte, string, bool)   { return nil, "", false }
func (storeAdapter) Base(string) (string, bool)           { return "", false }
func (storeAdapter) Route(*url.URL) (*url.URL, bool)      { return nil, false }
func (storeAdapter) Subscribe() (<-chan struct{}, func()) { return nil, func() {} }

// What each SSE client costs per change, with the shared snapshot (a cache hit).
func BenchmarkSnapshotCached(b *testing.B) {
	services := make([]registry.Service, 300)
	for i := range services {
		services[i] = registry.Service{Name: fmt.Sprintf("svc-%04d", i), Status: "up", SpecHash: "0123456789abcdef"}
	}
	st := benchStore{services}
	s := snapshotter{store: storeAdapter{st}}
	if _, err := s.get(); err != nil { // warm
		b.Fatal(err)
	}
	b.ReportAllocs()
	for b.Loop() {
		if _, err := s.get(); err != nil {
			b.Fatal(err)
		}
	}
}

// The same work done per client (the old behaviour), for comparison.
func BenchmarkSnapshotPerClient(b *testing.B) {
	services := make([]registry.Service, 300)
	for i := range services {
		services[i] = registry.Service{Name: fmt.Sprintf("svc-%04d", i), Status: "up", SpecHash: "0123456789abcdef"}
	}
	b.ReportAllocs()
	for b.Loop() {
		if _, err := json.Marshal(services); err != nil {
			b.Fatal(err)
		}
	}
}
