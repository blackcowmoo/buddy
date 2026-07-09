//go:build !embed

// Default build: nothing is embedded, so no dist/ directory needs to exist.
// In prod the server falls back to serving BUDDY_WEB_DIST from disk; in dev it
// proxies to Vite.
package webassets

import "io/fs"

// FS returns nil to signal "not embedded".
func FS() fs.FS { return nil }
