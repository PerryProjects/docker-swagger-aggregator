package registry_test

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/perryprojects/docker-swagger-aggregator/internal/registry"
)

func filled(n int) *registry.Registry {
	r := registry.New(registry.Config{})
	for i := range n {
		id := fmt.Sprintf("c%04d", i)
		r.Upsert(fmt.Sprintf("svc-%04d", i), id, true, "")
		r.SetSpec(id, []byte(`{"openapi":"3.0.0"}`))
	}
	return r
}

func BenchmarkListAndMarshal(b *testing.B) {
	r := filled(300)
	b.ReportAllocs()
	for b.Loop() {
		if _, err := json.Marshal(r.List()); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkSpecLookup(b *testing.B) {
	r := filled(300)
	b.ReportAllocs()
	for i := 0; b.Loop(); i++ {
		r.Spec(fmt.Sprintf("svc-%04d", i%300))
	}
}
