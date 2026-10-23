package discovery_test

import (
	"errors"
	"testing"

	"github.com/perryprojects/docker-swagger-aggregator/internal/discovery"
)

const p = discovery.DefaultPrefix

func TestParseTarget(t *testing.T) {
	t.Parallel()

	with := func(kv ...string) map[string]string {
		m := map[string]string{p + ".enable": "true"}
		for i := 0; i < len(kv); i += 2 {
			m[kv[i]] = kv[i+1]
		}
		return m
	}
	tests := []struct {
		name      string
		attrs     map[string]string
		fallbacks []string
		ports     []uint16
		want      discovery.Target
		wantErr   bool
	}{
		{
			"defaults", with(),
			[]string{"", "my-app"},
			[]uint16{8000, 9000},
			discovery.Target{Name: "my-app", Port: 8000, Path: "/openapi.json"},
			false,
		},
		{
			"first non-empty fallback wins", with(),
			[]string{"billing", "proj-billing-1"},
			[]uint16{80},
			discovery.Target{Name: "billing", Port: 80, Path: "/openapi.json"},
			false,
		},
		{
			"name attribute beats fallbacks", with(p+".name", "Billing API"),
			[]string{"billing"},
			[]uint16{80},
			discovery.Target{Name: "Billing API", Port: 80, Path: "/openapi.json"},
			false,
		},
		{
			"explicit port, path without slash, server-url",
			with(p+".port", "3000", p+".path", "docs/spec.yaml", p+".server-url", "https://api.example.com/v1"),
			[]string{"x"},
			nil,
			discovery.Target{Name: "x", Port: 3000, Path: "/docs/spec.yaml", ServerURL: "https://api.example.com/v1"},
			false,
		},
		{
			"group and order", with(p+".group", "payments", p+".order", "-2"),
			[]string{"x"},
			[]uint16{80},
			discovery.Target{Name: "x", Port: 80, Path: "/openapi.json", Group: "payments", Order: -2},
			false,
		},
		{
			"enable=false",
			map[string]string{p + ".enable": "false"},
			[]string{"x"},
			[]uint16{80},
			discovery.Target{},
			true,
		},
		{"no attributes", map[string]string{}, []string{"x"}, []uint16{80}, discovery.Target{}, true},
		{
			"the old bare key does not enable",
			map[string]string{p: "true"},
			[]string{"x"},
			[]uint16{80},
			discovery.Target{},
			true,
		},
		{"no name at all", with(), nil, []uint16{80}, discovery.Target{}, true},
		{"no port anywhere", with(), []string{"x"}, nil, discovery.Target{}, true},
		{"bad port", with(p+".port", "99999"), []string{"x"}, nil, discovery.Target{}, true},
		{"port zero", with(p+".port", "0"), []string{"x"}, nil, discovery.Target{}, true},
		{"bad server-url", with(p+".server-url", "ftp://nope"), []string{"x"}, []uint16{80}, discovery.Target{}, true},
		{"non-numeric order", with(p+".order", "first"), []string{"x"}, []uint16{80}, discovery.Target{}, true},
	}
	for _, tt := range tests {
		got, err := discovery.ParseTarget(p, tt.attrs, tt.fallbacks, tt.ports)
		if (err != nil) != tt.wantErr {
			t.Errorf("%s: err = %v, wantErr %v", tt.name, err, tt.wantErr)
			continue
		}
		if !tt.wantErr && got != tt.want {
			t.Errorf("%s: got %+v, want %+v", tt.name, got, tt.want)
		}
	}
	if _, err := discovery.ParseTarget(p, nil, []string{"x"}, nil); !errors.Is(err, discovery.ErrNotEnabled) {
		t.Errorf("want ErrNotEnabled, got %v", err)
	}
}

func TestParseTargetCustomPrefix(t *testing.T) {
	t.Parallel()

	attrs := map[string]string{
		"staging.docs.enable": "true", "staging.docs.port": "9000", "staging.docs.name": "Orders (staging)",
		p + ".enable": "true", // another instance's attributes must not matter
	}
	got, err := discovery.ParseTarget("staging.docs", attrs, nil, nil)
	want := discovery.Target{Name: "Orders (staging)", Port: 9000, Path: "/openapi.json"}
	if err != nil || got != want {
		t.Fatalf("got %+v, %v; want %+v", got, err, want)
	}
	if _, err = discovery.ParseTarget(
		"staging.docs",
		map[string]string{p + ".enable": "true"},
		[]string{"x"},
		[]uint16{80},
	); !errors.Is(
		err,
		discovery.ErrNotEnabled,
	) {
		t.Errorf("foreign attributes must not enable: %v", err)
	}
	if got := discovery.EnableKey("a.b"); got != "a.b.enable" {
		t.Errorf("EnableKey = %q", got)
	}
}
