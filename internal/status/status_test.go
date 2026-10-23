package status_test

import (
	"testing"

	"github.com/perryprojects/docker-swagger-aggregator/internal/status"
)

func TestCompute(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   status.Inputs
		want status.Status
	}{
		{"all good, no healthcheck", status.Inputs{Running: true, Health: "", ProbeOK: true}, status.Up},
		{"none health == no healthcheck", status.Inputs{Running: true, Health: "none", ProbeOK: true}, status.Up},
		{"healthy", status.Inputs{Running: true, Health: "healthy", ProbeOK: true}, status.Up},
		{"not running beats everything", status.Inputs{Running: false, Health: "healthy", ProbeOK: true}, status.Down},
		{"unhealthy beats good probe", status.Inputs{Running: true, Health: "unhealthy", ProbeOK: true}, status.Down},
		{"starting is not up yet", status.Inputs{Running: true, Health: "starting", ProbeOK: true}, status.Down},
		{"healthy but probe fails", status.Inputs{Running: true, Health: "healthy", ProbeOK: false}, status.Down},
		{"no healthcheck, probe fails", status.Inputs{Running: true, Health: "", ProbeOK: false}, status.Down},
	}
	for _, tt := range tests {
		if got := status.Compute(tt.in); got != tt.want {
			t.Errorf("%s: got %s, want %s", tt.name, got, tt.want)
		}
	}
}
