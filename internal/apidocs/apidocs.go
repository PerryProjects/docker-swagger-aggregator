// Package apidocs embeds and serves the documentation of this program's own HTTP interface as one OpenAPI 3.1
// document: the REST endpoints, the Server-Sent Events stream (/api/events) and the MCP endpoint (/mcp). A
// custom frontend is built against it; the UI shows it with Scalar.
//
// The document is a plain YAML file next to this file. Tests keep it in step with the code.
package apidocs

import (
	"embed"
	"encoding/json"
	"fmt"
	"net/http"

	"go.yaml.in/yaml/v3"
)

//go:embed openapi.yaml
var files embed.FS

// Handler serves GET /spec/openapi.json and /spec/openapi.yaml. Mount it at "/spec/".
func Handler() (http.Handler, error) {
	mux := http.NewServeMux()
	for _, name := range []string{"openapi"} {
		raw, err := files.ReadFile(name + ".yaml")
		if err != nil {
			return nil, err
		}
		var doc any
		if err = yaml.Unmarshal(raw, &doc); err != nil {
			return nil, fmt.Errorf("%s.yaml: %w", name, err)
		}
		asJSON, err := json.Marshal(doc)
		if err != nil {
			return nil, fmt.Errorf("%s.yaml: %w", name, err)
		}
		mux.Handle("GET /spec/"+name+".json", serve("application/json", asJSON))
		mux.Handle("GET /spec/"+name+".yaml", serve("application/yaml", raw))
	}
	return mux, nil
}

func serve(contentType string, body []byte) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", contentType)
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		_, _ = w.Write(body)
	})
}
