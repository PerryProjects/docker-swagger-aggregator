package discovery

import (
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
)

// DefaultPrefix is the default attribute namespace. A source reads attributes
// (Docker labels, Kubernetes annotations, ...) named `<prefix>.enable`,
// `<prefix>.port`, and so on. It can be changed to run several instances side
// by side, e.g. staging and production.
const DefaultPrefix = "swagger-aggregator"

const (
	keyEnable    = ".enable"
	keyPort      = ".port"
	keyPath      = ".path"
	keyName      = ".name"
	keyServerURL = ".server-url"
	keyGroup     = ".group"
	keyOrder     = ".order"
	keyStrip     = ".strip-prefix"
	keyScheme    = ".scheme"

	defaultPath = "/openapi.json"
)

// EnableKey returns the attribute that switches discovery on for a workload.
func EnableKey(prefix string) string { return prefix + keyEnable }

// ErrNotEnabled means the workload is not labeled `<prefix>.enable=true`.
var ErrNotEnabled = errors.New("workload not enabled")

// Target is what a workload's attributes say about how to document it.
type Target struct {
	Name      string
	Port      uint16
	Path      string
	ServerURL string
	Group     string
	Order     int
	// KeepServerPath: forward the server-url path as is (the workload serves under it);
	// the default replaces it by the workload root, like a proxy that strips the prefix.
	KeepServerPath bool
	// Scheme is how the workload itself is reached: "http" (default) or "https".
	Scheme string
}

// ParseTarget interprets a workload's attributes. nameFallbacks are tried in
// order when there is no `<prefix>.name`; exposedPorts must be sorted ascending
// and the first one is the default port.
func ParseTarget(
	prefix string,
	attrs map[string]string,
	nameFallbacks []string,
	exposedPorts []uint16,
) (Target, error) {
	if attrs[EnableKey(prefix)] != "true" {
		return Target{}, ErrNotEnabled
	}
	t := Target{Path: defaultPath, Group: attrs[prefix+keyGroup], Scheme: "http"}

	t.Name = first(append([]string{attrs[prefix+keyName]}, nameFallbacks...)...)
	if t.Name == "" {
		return t, errors.New("cannot determine a service name")
	}
	var err error
	if t.Port, err = parsePort(prefix, attrs, exposedPorts); err != nil {
		return t, err
	}
	if p := attrs[prefix+keyPath]; p != "" {
		t.Path = "/" + strings.TrimPrefix(p, "/")
	}
	if t.ServerURL, err = parseServerURL(prefix, attrs); err != nil {
		return t, err
	}
	if t.KeepServerPath, err = parseKeepServerPath(prefix, attrs); err != nil {
		return t, err
	}
	if s := attrs[prefix+keyScheme]; s != "" {
		if s != "http" && s != "https" {
			return t, fmt.Errorf("invalid %s%s %q (http or https)", prefix, keyScheme, s)
		}
		t.Scheme = s
	}
	if o := attrs[prefix+keyOrder]; o != "" {
		if t.Order, err = strconv.Atoi(o); err != nil {
			return t, fmt.Errorf("invalid %s%s %q", prefix, keyOrder, o)
		}
	}
	return t, nil
}

// parsePort uses the port attribute, else the first exposed port.
func parsePort(prefix string, attrs map[string]string, exposedPorts []uint16) (uint16, error) {
	if p := attrs[prefix+keyPort]; p != "" {
		n, err := strconv.ParseUint(p, 10, 16)
		if err != nil || n == 0 {
			return 0, fmt.Errorf("invalid %s%s %q", prefix, keyPort, p)
		}
		return uint16(n), nil
	}
	if len(exposedPorts) > 0 {
		return exposedPorts[0], nil
	}
	return 0, fmt.Errorf("no exposed port and no %s%s", prefix, keyPort)
}

func parseServerURL(prefix string, attrs map[string]string) (string, error) {
	s := attrs[prefix+keyServerURL]
	if s == "" {
		return "", nil
	}
	u, err := url.Parse(s)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return "", fmt.Errorf("invalid %s%s %q", prefix, keyServerURL, s)
	}
	return s, nil
}

// parseKeepServerPath reports whether the server-url path must be forwarded unchanged (strip-prefix=false).
func parseKeepServerPath(prefix string, attrs map[string]string) (bool, error) {
	s := attrs[prefix+keyStrip]
	if s == "" {
		return false, nil
	}
	strip, err := strconv.ParseBool(s)
	if err != nil {
		return false, fmt.Errorf("invalid %s%s %q (true or false)", prefix, keyStrip, s)
	}
	return !strip, nil
}

func first(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
