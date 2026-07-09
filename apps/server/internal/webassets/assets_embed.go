//go:build embed

// This file is only compiled with `-tags embed` (the Docker build does this
// after copying apps/web/dist into ./dist here). The result is a single,
// self-contained binary that serves the frontend from memory — no static dir
// needed at runtime.
package webassets

import (
	"embed"
	"io/fs"
)

//go:embed all:dist
var dist embed.FS

// FS returns the embedded frontend build (the contents of dist/).
func FS() fs.FS {
	sub, err := fs.Sub(dist, "dist")
	if err != nil {
		panic(err) // build is misconfigured: dist/ was not populated
	}
	return sub
}
