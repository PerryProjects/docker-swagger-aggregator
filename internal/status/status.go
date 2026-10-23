// Package status derives a service's up/down state from the available signals.
package status

type Status string

const (
	Up   Status = "up"
	Down Status = "down"
)

// Inputs are the raw signals. Health is Docker's health status
// ("", "none", "starting", "healthy", "unhealthy"); empty/"none" means the
// container has no HEALTHCHECK.
type Inputs struct {
	Running bool
	Health  string
	ProbeOK bool // last HTTP fetch of the spec URL succeeded
}

// Compute is deliberately pessimistic: every available signal must be
// positive, so the most negative signal wins.
//
//  1. container not running      -> down (hard stop, nothing else matters)
//  2. HEALTHCHECK present        -> must be "healthy" ("starting"/"unhealthy" = down)
//  3. HTTP probe of the spec URL -> must have succeeded
func Compute(in Inputs) Status {
	if !in.Running {
		return Down
	}
	switch in.Health {
	case "", "none", "healthy":
	default:
		return Down
	}
	if !in.ProbeOK {
		return Down
	}
	return Up
}
