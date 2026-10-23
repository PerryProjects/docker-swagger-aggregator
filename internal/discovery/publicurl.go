package discovery

import (
	"regexp"
	"strings"
)

// hostOnlyRule matches exactly `Host(`x`)`: anything with a path, regexp or
// boolean operators is ambiguous (a PathPrefix would need to know whether a
// StripPrefix middleware removes it before the request reaches the workload).
var hostOnlyRule = regexp.MustCompile(
	"^\\s*Host\\(\\s*[`\"]([^`\"]+)[`\"]\\s*\\)\\s*$",
)

// HostOnlyRule extracts the host from a Traefik router rule that is exactly
// Host(`x`). Wildcard-ish hosts are rejected.
func HostOnlyRule(rule string) (string, bool) {
	m := hostOnlyRule.FindStringSubmatch(rule)
	if m == nil || strings.ContainsAny(m[1], "*{}") {
		return "", false
	}
	return m[1], true
}

// SingleURL returns the only element of urls, or false if there are none or
// several: deriving a public URL is only done when it is unambiguous.
func SingleURL(urls map[string]bool) (string, bool) {
	if len(urls) != 1 {
		return "", false
	}
	for u := range urls {
		return u, true
	}
	return "", false
}
