package httpserver

import (
	"io/fs"
	"net/http"
	"path"
	"strings"
)

// spaHandlerFS serves a built frontend from any fs.FS (embedded or os.DirFS),
// falling back to index.html for client-side routes (SPA history mode).
func spaHandlerFS(fsys fs.FS) http.Handler {
	fileServer := http.FileServerFS(fsys)
	index, _ := fs.ReadFile(fsys, "index.html")

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimPrefix(path.Clean(r.URL.Path), "/")
		if name != "" && name != "." {
			if info, err := fs.Stat(fsys, name); err == nil && !info.IsDir() {
				fileServer.ServeHTTP(w, r) // real asset
				return
			}
		}
		if index == nil {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(index)
	})
}
