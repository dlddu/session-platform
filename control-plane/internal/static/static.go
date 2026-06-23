// Package static serves the built React SPA that `vite build` emits into
// ./dist. The directory is embedded into the control plane binary so a single
// image serves both the API and the UI (one port).
//
// During `make build` the web app is built first and its output copied into
// this package's dist/ directory; a committed placeholder index.html keeps the
// embed (and therefore `go build`) working before the web build has run.
package static

import (
	"embed"
	"io/fs"
	"net/http"
	"strings"
)

//go:embed all:dist
var embedded embed.FS

// Handler returns an http.Handler that serves the SPA: static assets are served
// directly, and any non-API, non-asset path falls back to index.html so client
// side routing works.
func Handler() http.Handler {
	sub, err := fs.Sub(embedded, "dist")
	if err != nil {
		panic(err)
	}
	fileServer := http.FileServer(http.FS(sub))

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Never let the SPA shadow the API.
		if strings.HasPrefix(r.URL.Path, "/api/") {
			http.NotFound(w, r)
			return
		}
		// If the requested file exists, serve it; otherwise fall back to the
		// SPA entrypoint (history-mode routing).
		p := strings.TrimPrefix(r.URL.Path, "/")
		if p == "" {
			p = "index.html"
		}
		if _, err := fs.Stat(sub, p); err != nil {
			r2 := r.Clone(r.Context())
			r2.URL.Path = "/"
			http.ServeFileFS(w, r2, sub, "index.html")
			return
		}
		fileServer.ServeHTTP(w, r)
	})
}
