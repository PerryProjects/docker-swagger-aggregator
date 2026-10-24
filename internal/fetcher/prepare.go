package fetcher

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"

	"go.yaml.in/yaml/v3"
)

// Prepare parses raw (JSON, else YAML), checks that it is an OpenAPI 3.x or
// Swagger 2.0 document and returns canonical JSON (sorted keys, so the hash is
// stable). The server location is rewritten so that "Try it" works from a browser:
//   - serverURL set: that URL is used as is (the browser calls it directly, so CORS applies);
//   - else proxyPath set: a same-origin path that the API reverse-proxies to the container.
func Prepare(raw []byte, serverURL, proxyPath string) ([]byte, error) {
	doc, err := parse(raw)
	if err != nil {
		return nil, err
	}
	v2 := doc["swagger"] == "2.0"
	if v, _ := doc["openapi"].(string); !v2 && !strings.HasPrefix(v, "3.") {
		return nil, errors.New("not an OpenAPI 3.x or Swagger 2.0 document")
	}
	if serverURL != "" || proxyPath != "" {
		if err = rewrite(doc, serverURL, proxyPath, v2); err != nil {
			return nil, err
		}
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err = enc.Encode(doc); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

func parse(raw []byte) (map[string]any, error) {
	var doc map[string]any
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber() // keep large ints intact
	if err := dec.Decode(&doc); err == nil {
		return doc, nil
	}
	var y any
	if err := yaml.Unmarshal(raw, &y); err != nil {
		return nil, fmt.Errorf("neither valid JSON nor YAML: %w", err)
	}
	doc, ok := stringKeys(y).(map[string]any)
	if !ok {
		return nil, errors.New("spec root is not an object")
	}
	return doc, nil
}

// stringKeys makes YAML output JSON-encodable: YAML allows non-string keys
// (e.g. unquoted `200:` response codes), encoding/json does not.
func stringKeys(v any) any {
	switch t := v.(type) {
	case map[string]any:
		for k, e := range t {
			t[k] = stringKeys(e)
		}
		return t
	case map[any]any:
		m := make(map[string]any, len(t))
		for k, e := range t {
			m[fmt.Sprint(k)] = stringKeys(e)
		}
		return m
	case []any:
		for i, e := range t {
			t[i] = stringKeys(e)
		}
	}
	return v
}

// rewrite points the spec at serverURL or, if empty, at proxyPath. When proxying,
// the path of the original server (e.g. /v1) is kept as a suffix.
// OpenAPI 3 path- and operation-level `servers` overrides are rewritten the same way.
func rewrite(doc map[string]any, serverURL, proxyPath string, v2 bool) error {
	if serverURL == "" {
		return rewriteToProxy(doc, proxyPath, v2)
	}
	u, err := url.Parse(serverURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("invalid server-url %q", serverURL)
	}
	if !v2 {
		rewriteServers(doc, func(any) []any { return []any{map[string]any{"url": serverURL}} })
		doc["servers"] = []any{map[string]any{"url": serverURL}}
		return nil
	}
	doc["host"] = u.Host
	doc["basePath"] = "/" + strings.Trim(u.Path, "/")
	doc["schemes"] = []any{u.Scheme}
	return nil
}

func rewriteToProxy(doc map[string]any, proxyPath string, v2 bool) error {
	if v2 {
		// Without host/schemes Swagger 2.0 means "the host that served this spec".
		prefix, _ := doc["basePath"].(string)
		delete(doc, "host")
		delete(doc, "schemes")
		doc["basePath"] = proxyPath + cleanPrefix(prefix)
		return nil
	}
	toProxy := func(servers any) []any {
		var prefix string
		if l, ok := servers.([]any); ok && len(l) > 0 {
			first, _ := l[0].(map[string]any)
			raw, _ := first["url"].(string)
			prefix = pathOf(raw)
		}
		return []any{map[string]any{"url": proxyPath + cleanPrefix(prefix)}}
	}
	rewriteServers(doc, toProxy)
	doc["servers"] = toProxy(doc["servers"])
	return nil
}

var httpMethods = []string{"get", "put", "post", "delete", "options", "head", "patch", "trace"}

// rewriteServers replaces the `servers` of every path item and operation that has its own.
func rewriteServers(doc map[string]any, replace func(old any) []any) {
	paths, _ := doc["paths"].(map[string]any)
	for _, pi := range paths {
		item, ok := pi.(map[string]any)
		if !ok {
			continue
		}
		if old, has := item["servers"]; has {
			item["servers"] = replace(old)
		}
		for _, m := range httpMethods {
			if op, ok := item[m].(map[string]any); ok {
				if old, has := op["servers"]; has {
					op["servers"] = replace(old)
				}
			}
		}
	}
}

// pathOf returns the path of an absolute or relative server URL.
func pathOf(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return u.Path
}

// cleanPrefix normalises to "" or "/a/b". Templated paths ({var}) are dropped
// because the variable definitions are replaced along with the server.
func cleanPrefix(p string) string {
	p = strings.Trim(p, "/")
	if p == "" || strings.ContainsAny(p, "{}") {
		return ""
	}
	return "/" + p
}
