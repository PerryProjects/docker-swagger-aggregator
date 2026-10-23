package fetcher_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/perryprojects/docker-swagger-aggregator/internal/fetcher"
)

func bigSpec(paths int) []byte {
	var sb strings.Builder
	sb.WriteString(
		`{"openapi":"3.0.3","info":{"title":"big","version":"1"},"servers":[{"url":"http://internal:8000"}],"paths":{`,
	)
	for i := range paths {
		if i > 0 {
			sb.WriteByte(',')
		}
		fmt.Fprintf(
			&sb,
			`"/items/%d":{"get":{"summary":"item %d","responses":{"200":{"description":"ok","content":{"application/json":{"schema":{"type":"object","properties":{"id":{"type":"integer"},"name":{"type":"string"}}}}}}}}}`,
			i,
			i,
		)
	}
	sb.WriteString(`}}`)
	return []byte(sb.String())
}

func BenchmarkPrepare(b *testing.B) {
	raw := bigSpec(2000) // ~0.8 MB
	b.SetBytes(int64(len(raw)))
	b.ReportAllocs()
	for b.Loop() {
		if _, err := fetcher.Prepare(raw, "", "/api/proxy/c1"); err != nil {
			b.Fatal(err)
		}
	}
}
