package web

import (
	"embed"
	"io/fs"
	"net/http"
	"path"
	"strings"
)

// distFS embeds the built Svelte SPA. The directory always contains at least a
// committed placeholder index.html, so a bare `go build` (no `make web-ui`)
// still compiles and serves a usable page.
//
//go:embed all:ui/dist
var distFS embed.FS

// spaHandler serves the embedded SPA: static assets by path, with a fallback to
// index.html for any unknown path (client-side routing). It never serves
// directory listings and never escapes the embedded tree.
func spaHandler() http.Handler {
	sub, err := fs.Sub(distFS, "ui/dist")
	if err != nil {
		// Embedding guarantees ui/dist exists; this is unreachable in a built
		// binary, but fail closed rather than panic.
		return http.NotFoundHandler()
	}
	fileServer := http.FileServer(http.FS(sub))

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		clean := strings.TrimPrefix(path.Clean(r.URL.Path), "/")
		if clean == "" {
			clean = "index.html"
		}
		if _, err := fs.Stat(sub, clean); err != nil {
			// Unknown path → SPA route: hand back index.html so the client router
			// resolves it. Re-point the request at the root.
			r2 := r.Clone(r.Context())
			r2.URL.Path = "/"
			fileServer.ServeHTTP(w, r2)
			return
		}
		fileServer.ServeHTTP(w, r)
	})
}
