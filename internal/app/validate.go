package app

import (
	"fmt"
	"net"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/perryprojects/docker-swagger-aggregator/internal/discovery/kubernetes"
)

// Limits. They reject accidents (a missing unit, a zero, a typo'd magnitude), not legitimate tuning.
const (
	minInterval    = 10 * time.Millisecond
	maxProbe       = 24 * time.Hour
	maxFetch       = 10 * time.Minute
	maxGrace       = 10 * time.Minute
	minSpecBytes   = 1 << 10
	maxSpecBytes   = 1 << 30
	maxDNSLabel    = 63
	maxDNSName     = 253
	maxDockerName  = 255
	longestSuffix  = len(".server-url") // the longest attribute name built from the prefix
	maxPrefixBytes = maxDNSLabel - longestSuffix
)

var (
	// A label key's name part: alphanumerics, '-', '_' and '.', starting and ending alphanumeric.
	labelPrefixRe = regexp.MustCompile(`^[A-Za-z0-9]([-A-Za-z0-9_.]*[A-Za-z0-9])?$`)
	dnsLabelRe    = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)
	dockerNameRe  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]*$`)
)

// FieldError describes one invalid setting. Field is the flag name, so the message maps to what the user typed.
type FieldError struct {
	Field   string
	Message string
}

func (e FieldError) Error() string { return "--" + e.Field + ": " + e.Message }

// ValidationError lists every problem found, not just the first.
type ValidationError struct {
	Problems []FieldError
}

func (e *ValidationError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "invalid configuration (%d problem", len(e.Problems))
	if len(e.Problems) != 1 {
		b.WriteString("s")
	}
	b.WriteString("):")
	for _, p := range e.Problems {
		b.WriteString("\n  - " + p.Error())
	}
	return b.String()
}

// Unwrap exposes the individual problems to [errors.Is] and [errors.As].
func (e *ValidationError) Unwrap() []error {
	out := make([]error, len(e.Problems))
	for i, p := range e.Problems {
		out[i] = p
	}
	return out
}

type collector struct{ problems []FieldError }

func (c *collector) addf(field, format string, args ...any) {
	c.problems = append(c.problems, FieldError{Field: field, Message: fmt.Sprintf(format, args...)})
}

func (c *collector) err() error {
	if len(c.problems) == 0 {
		return nil
	}
	return &ValidationError{Problems: c.problems}
}

// Validate checks the whole configuration and returns a *ValidationError listing every problem.
func (c Config) Validate() error {
	var v collector
	c.validateCommon(&v)
	c.validateDocker(&v)
	c.validateKubernetes(&v)
	return v.err()
}

func (c Config) validateCommon(v *collector) {
	validateListen(v, c.Listen)
	if c.BasePath != "" && (!strings.HasPrefix(c.BasePath, "/") || strings.HasSuffix(c.BasePath, "/") ||
		strings.ContainsAny(c.BasePath, "?# ")) {
		v.addf("base-path", "%q must look like /docs (leading slash, no trailing slash)", c.BasePath)
	}

	switch {
	case c.LabelPrefix == "":
		v.addf("label-prefix", "must not be empty")
	case len(c.LabelPrefix) > maxPrefixBytes:
		v.addf(
			"label-prefix",
			"%d characters is too long: label names are limited to %d characters including a suffix like %q, so at most %d",
			len(c.LabelPrefix),
			maxDNSLabel,
			".server-url",
			maxPrefixBytes,
		)
	case !labelPrefixRe.MatchString(c.LabelPrefix):
		v.addf(
			"label-prefix",
			"%q is not a valid label name (letters, digits, '-', '_' and '.', starting and ending with a letter or digit)",
			c.LabelPrefix,
		)
	}

	validateRange(v, "probe-interval", c.ProbeInterval, minInterval, maxProbe)
	validateRange(v, "fetch-timeout", c.FetchTimeout, minInterval, maxFetch)
	validateRange(v, "shutdown-grace", c.ShutdownGrace, minInterval, maxGrace)
	if c.MaxSpecBytes < minSpecBytes || c.MaxSpecBytes > maxSpecBytes {
		v.addf(
			"max-spec-bytes",
			"%d is out of range: must be between %d (1 KiB) and %d (1 GiB)",
			c.MaxSpecBytes,
			minSpecBytes,
			maxSpecBytes,
		)
	}

	if !c.DockerEnabled && !c.Kubernetes.Enabled {
		v.addf("docker", "no source is enabled: turn on --docker (default) or --kubernetes")
	}
}

func validateListen(v *collector, listen string) {
	if listen == "" {
		v.addf("listen", "must not be empty (e.g. :8080 or 127.0.0.1:8080)")
		return
	}
	_, port, err := net.SplitHostPort(listen)
	if err != nil {
		v.addf("listen", "%q is not host:port (e.g. :8080 or 127.0.0.1:8080)", listen)
		return
	}
	if n, perr := strconv.ParseUint(port, 10, 16); perr != nil {
		v.addf("listen", "port %q is not a number between 0 and 65535", port)
	} else if n == 0 {
		v.addf("listen", "port 0 would pick a random port; choose a fixed one")
	}
}

func validateRange(v *collector, field string, d, lo, hi time.Duration) {
	if d < lo || d > hi {
		v.addf(field, "%s is out of range: must be between %s and %s", d, lo, hi)
	}
}

func (c Config) validateDocker(v *collector) {
	if !c.DockerEnabled {
		// Options for a disabled source are almost always a mistake (forgotten flag, wrong environment).
		noEffect(v, "docker", []flagUse{
			{"docker-host", c.DockerHost != ""},
			{"network", c.Network != ""},
			{"prefer-published-ports", c.PreferPublishedPorts},
		})
		return
	}
	if c.DockerHost != "" {
		u, err := url.Parse(c.DockerHost)
		if err != nil || u.Scheme == "" ||
			!slices.Contains([]string{"tcp", "unix", "npipe", "ssh", "http", "https", "fd"}, u.Scheme) {
			v.addf(
				"docker-host",
				"%q must look like tcp://host:2375, unix:///var/run/docker.sock, npipe://... or ssh://...",
				c.DockerHost,
			)
		}
	}
	if c.Network != "" && (len(c.Network) > maxDockerName || !dockerNameRe.MatchString(c.Network)) {
		v.addf("network", "%q is not a valid Docker network name", c.Network)
	}
}

func (c Config) validateKubernetes(v *collector) {
	k := c.Kubernetes
	if !k.Enabled {
		noEffect(v, "kubernetes", []flagUse{
			{"kubeconfig", k.Kubeconfig != ""},
			{"kube-context", k.Context != ""},
			{"namespace", len(k.Namespaces) > 0},
		})
		return
	}
	if k.Access != accessAuto && k.Access != string(kubernetes.AccessService) &&
		k.Access != string(kubernetes.AccessAPIServer) {
		v.addf("kube-access", "%q is not one of auto, service, apiserver", k.Access)
	}
	if !validDNSName(k.ClusterDomain) {
		v.addf("cluster-domain", "%q is not a valid DNS name (e.g. cluster.local)", k.ClusterDomain)
	}
	seen := map[string]bool{}
	for _, ns := range k.Namespaces {
		switch {
		case len(ns) > maxDNSLabel || !dnsLabelRe.MatchString(ns):
			v.addf(
				"namespace",
				"%q is not a valid namespace name (lowercase letters, digits and '-', at most %d characters)",
				ns,
				maxDNSLabel,
			)
		case seen[ns]:
			v.addf("namespace", "%q is given more than once", ns)
		}
		seen[ns] = true
	}
}

// validDNSName reports whether s is a DNS-1123 subdomain: dot separated lowercase labels.
func validDNSName(s string) bool {
	if s == "" || len(s) > maxDNSName {
		return false
	}
	for label := range strings.SplitSeq(s, ".") {
		if len(label) > maxDNSLabel || !dnsLabelRe.MatchString(label) {
			return false
		}
	}
	return true
}

// flagUse says whether an option of a source was given.
type flagUse struct {
	field string
	set   bool
}

// noEffect reports options of a disabled source: almost always a mistake (forgotten flag, wrong environment).
func noEffect(v *collector, source string, uses []flagUse) {
	for _, u := range uses {
		if u.set {
			v.addf(u.field, "has no effect because --%s is disabled", source)
		}
	}
}
