package api

import (
	"io"
	"net/http"
)

// handleOpenAPI serves the embedded OpenAPI 3.0 specification, consumed by
// the embedded Swagger UI at /docs and usable by any external OpenAPI
// tooling.
func (s *Server) handleOpenAPI(w http.ResponseWriter, r *http.Request) {
	data, err := staticAssets.Open("openapi.json")
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer data.Close()
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_, _ = io.Copy(w, data)
}
