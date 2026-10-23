// Package transport routes outgoing requests to different RoundTrippers by host.
package transport

import "net/http"

// Mux sends a request through the RoundTripper registered for its URL host (as
// in URL.Host, with port) and everything else through Default. It lets one
// authenticated destination, e.g. the Kubernetes API server proxy, coexist with
// plain HTTP to everything else, without per-workload transports.
type Mux struct {
	// Default handles hosts without an entry. nil means http.DefaultTransport.
	Default http.RoundTripper
	// ByHost maps URL.Host to the RoundTripper for that host.
	ByHost map[string]http.RoundTripper
}

// RoundTrip implements [http.RoundTripper].
func (m *Mux) RoundTrip(req *http.Request) (*http.Response, error) {
	if rt, ok := m.ByHost[req.URL.Host]; ok {
		return rt.RoundTrip(req)
	}
	if m.Default != nil {
		return m.Default.RoundTrip(req)
	}
	return http.DefaultTransport.RoundTrip(req)
}
