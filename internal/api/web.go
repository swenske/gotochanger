package api

import (
	"embed"
	"io"
	"io/fs"
	"net/http"
)

//go:embed static
var staticFS embed.FS

var staticAssets, _ = fs.Sub(staticFS, "static")

// RegisterWebUI mounts the embedded single-page management UI at "/" and its
// static assets under "/assets/". The UI itself talks to the JSON API using
// a token entered by the operator and stored in the browser, so it does not
// need special-casing beyond being reachable without a token.
func RegisterWebUI(mux *http.ServeMux) {
	fileServer := http.FileServer(http.FS(staticAssets))
	mux.Handle("GET /assets/", http.StripPrefix("/assets/", fileServer))
	mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		data, _ := staticAssets.Open("index.html")
		defer data.Close()
		_, _ = io.Copy(w, data)
	})
}

// RegisterSwaggerUI mounts the embedded Swagger UI at "/docs", pointed at
// the live OpenAPI spec served at /api/v1/openapi.json. The static assets
// it depends on (swagger-ui-bundle.js, swagger-ui.css, ...) are already
// reachable under /assets/swagger/ via RegisterWebUI's file server.
func RegisterSwaggerUI(mux *http.ServeMux) {
	mux.HandleFunc("GET /docs", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		data, err := staticAssets.Open("swagger/index.html")
		if err != nil {
			http.NotFound(w, r)
			return
		}
		defer data.Close()
		_, _ = io.Copy(w, data)
	})
}

// RegisterUserGuide mounts the embedded, self-contained user guide at
// "/guide". It opens in a new tab from the main nav, exactly like
// RegisterSwaggerUI's /docs - its own CSS lives under static/guide/ and is
// already reachable at /assets/guide/... via RegisterWebUI's file server.
func RegisterUserGuide(mux *http.ServeMux) {
	mux.HandleFunc("GET /guide", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		data, err := staticAssets.Open("guide/index.html")
		if err != nil {
			http.NotFound(w, r)
			return
		}
		defer data.Close()
		_, _ = io.Copy(w, data)
	})
}
