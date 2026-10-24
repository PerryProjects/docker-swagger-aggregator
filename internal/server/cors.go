package server

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// ParseCORSOrigins validates the origins a frontend may be served from and returns them normalised (lower case).
// An origin is scheme://host[:port]: no path, and no wildcard, because the browser only sends cookies to an
// explicitly named origin.
func ParseCORSOrigins(in []string) ([]string, error) {
	var out []string
	for _, o := range in {
		o = strings.ToLower(strings.TrimSpace(o))
		if o == "" {
			continue
		}
		u, err := url.Parse(o)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.Path != "" || u.RawQuery != "" ||
			u.Fragment != "" || u.User != nil || strings.Contains(u.Host, "*") {
			return nil, fmt.Errorf("%q is not an origin: use scheme://host[:port] (no path, no wildcard), e.g. https://app.example.com", o)
		}
		out = append(out, u.Scheme+"://"+u.Host)
	}
	return out, nil
}

// corsMethods and corsHeaders are what a preflight may ask for: nothing beyond what the UI itself uses.
const corsMethods = "GET, POST, PUT, PATCH, DELETE, HEAD, OPTIONS"

var corsHeaders = map[string]bool{"content-type": true, "accept": true, "if-none-match": true, "if-modified-since": true}

// CORS lets browsers on the listed origins use the interface with the session cookie (credentials), and protects it
// from the others. origins must come from ParseCORSOrigins; nil means "same origin only": the middleware does nothing.
//
//   - A request from a listed origin gets the CORS headers (with credentials), a preflight is answered at once.
//   - A preflight from any other origin is refused.
//   - Requests that change something (not GET/HEAD/OPTIONS) from another origin that is not listed are refused with 403.
//     With the cookie relaxed to SameSite=None the browser no longer keeps foreign sites from sending it, so the
//     server checks the Origin header itself. Requests without an Origin header (curl, servers) are not browsers
//     and pass; so do requests of the origin the server itself is reached at.
func CORS(origins []string) func(http.Handler) http.Handler {
	if len(origins) == 0 {
		return func(next http.Handler) http.Handler { return next }
	}
	allowed := make(map[string]bool, len(origins))
	for _, o := range origins {
		allowed[o] = true
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			h := w.Header()
			h.Add("Vary", "Origin")
			origin := r.Header.Get("Origin")
			if origin == "" {
				next.ServeHTTP(w, r)
				return
			}
			listed := allowed[strings.ToLower(origin)]
			preflight := r.Method == http.MethodOptions && r.Header.Get("Access-Control-Request-Method") != ""
			switch {
			case listed:
				h.Set("Access-Control-Allow-Origin", origin)
				h.Set("Access-Control-Allow-Credentials", "true")
				h.Set("Access-Control-Expose-Headers", "ETag, Retry-After")
				if preflight {
					h.Add("Vary", "Access-Control-Request-Headers")
					h.Set("Access-Control-Allow-Methods", corsMethods)
					h.Set("Access-Control-Allow-Headers", allowedRequestHeaders(r.Header.Get("Access-Control-Request-Headers")))
					h.Set("Access-Control-Max-Age", "600")
					w.WriteHeader(http.StatusNoContent)
					return
				}
			case preflight:
				http.Error(w, "origin not allowed", http.StatusForbidden)
				return
			case unsafeMethod(r.Method) && !sameOrigin(r, origin):
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusForbidden)
				_, _ = w.Write([]byte(`{"error":"cross-origin request refused: the origin is not in the CORS list"}` + "\n"))
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

func unsafeMethod(m string) bool {
	return m != http.MethodGet && m != http.MethodHead && m != http.MethodOptions
}

// sameOrigin reports whether the Origin header names the host this request was addressed to.
func sameOrigin(r *http.Request, origin string) bool {
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" {
		return false // "null" and malformed values
	}
	host := strings.ToLower(u.Host)
	if strings.ToLower(r.Host) == host {
		return true
	}
	fwd, _, _ := strings.Cut(r.Header.Get("X-Forwarded-Host"), ",")
	return strings.ToLower(strings.TrimSpace(fwd)) == host
}

// allowedRequestHeaders echoes the requested headers that are on the list; others are left out and so blocked.
func allowedRequestHeaders(requested string) string {
	var out []string
	for _, name := range strings.Split(requested, ",") {
		if name = strings.TrimSpace(name); corsHeaders[strings.ToLower(name)] {
			out = append(out, name)
		}
	}
	return strings.Join(out, ", ")
}
