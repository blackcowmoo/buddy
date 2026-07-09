package httpserver

import (
	"net/http"
	"net/http/httputil"
	"net/url"
)

// viteProxy reverse-proxies everything the API doesn't handle to the Vite dev
// server. Go's ReverseProxy transparently forwards WebSocket upgrades, so
// Vite's HMR works through this single origin — no nginx required.
//
// (Set server.hmr.clientPort in vite.config.ts to this server's port so the
// browser opens its HMR socket back through here.)
func viteProxy(target string) (http.Handler, error) {
	u, err := url.Parse(target)
	if err != nil {
		return nil, err
	}
	proxy := httputil.NewSingleHostReverseProxy(u)
	// Preserve the original director but fix the Host header for Vite.
	orig := proxy.Director
	proxy.Director = func(r *http.Request) {
		orig(r)
		r.Host = u.Host
	}
	return proxy, nil
}
