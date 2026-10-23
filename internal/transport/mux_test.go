package transport_test

import (
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/perryprojects/docker-swagger-aggregator/internal/transport"
)

type tagRT string

func (t tagRT) RoundTrip(*http.Request) (*http.Response, error) {
	return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(string(t)))}, nil
}

func TestMuxRoutesByHost(t *testing.T) {
	t.Parallel()

	m := &transport.Mux{
		Default: tagRT("default"),
		ByHost:  map[string]http.RoundTripper{"127.0.0.1:6443": tagRT("apiserver")},
	}
	for url, want := range map[string]string{
		"https://127.0.0.1:6443/api/v1/x": "apiserver",
		"https://127.0.0.1:6444/api/v1/x": "default", // other port: different host
		"http://orders:8000/openapi.json": "default",
	} {
		req, _ := http.NewRequest(http.MethodGet, url, nil)
		resp, err := m.RoundTrip(req)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		if string(body) != want {
			t.Errorf("%s -> %s, want %s", url, body, want)
		}
	}
}
