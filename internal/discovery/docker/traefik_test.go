package docker_test

import (
	"testing"

	"github.com/perryprojects/docker-swagger-aggregator/internal/discovery/docker"
)

func TestTraefikServerURL(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		labels map[string]string
		want   string // "" = no derivation
	}{
		{"host + tls=true", map[string]string{
			"traefik.http.routers.api.rule": "Host(`api.example.com`)",
			"traefik.http.routers.api.tls":  "true",
		}, "https://api.example.com"},
		{"host + certresolver counts as tls", map[string]string{
			"traefik.http.routers.api.rule":             "Host(`api.example.com`)",
			"traefik.http.routers.api.tls.certresolver": "le",
		}, "https://api.example.com"},
		{"http and https routers, same host", map[string]string{
			"traefik.http.routers.api.rule":        "Host(`api.example.com`)",
			"traefik.http.routers.api-sec.rule":    "Host(`api.example.com`)",
			"traefik.http.routers.api-sec.tls":     "true",
			"traefik.http.routers.api.entrypoints": "web",
		}, "https://api.example.com"},
		{
			"no tls label: scheme unknown",
			map[string]string{"traefik.http.routers.api.rule": "Host(`api.example.com`)"},
			"",
		},
		{"path prefix is ambiguous", map[string]string{
			"traefik.http.routers.api.rule": "Host(`a.example.com`) && PathPrefix(`/v1`)",
			"traefik.http.routers.api.tls":  "true",
		}, ""},
		{"regexp / or rules are not handled", map[string]string{
			"traefik.http.routers.api.rule": "Host(`a.example.com`) || Host(`b.example.com`)",
			"traefik.http.routers.api.tls":  "true",
		}, ""},
		{"two different hosts", map[string]string{
			"traefik.http.routers.a.rule": "Host(`a.example.com`)", "traefik.http.routers.a.tls": "true",
			"traefik.http.routers.b.rule": "Host(`b.example.com`)", "traefik.http.routers.b.tls": "true",
		}, ""},
		{"no traefik labels", map[string]string{"foo": "bar"}, ""},
	}
	for _, tt := range tests {
		got, ok := docker.TraefikServerURL(tt.labels)
		if got != tt.want || ok != (tt.want != "") {
			t.Errorf("%s: got (%q, %v), want %q", tt.name, got, ok, tt.want)
		}
	}
}
