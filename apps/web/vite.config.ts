import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";

// Single origin: the browser always talks to the Go server on :8080, which
// proxies HTML/HMR here in dev. So the HMR socket must report back through
// :8080, not Vite's own port.
//
// base: "./" (build only) makes built asset URLs relative instead of
// root-absolute, so the same embedded frontend works whether the Go server
// is mounted at "/" or under a ROOT_PATH prefix like "/pr/14" (see
// httpserver.withRootPath) — the browser resolves them against whatever path
// index.html was served from. Left as "/" in dev since the Vite dev server
// (and its HMR client) isn't proxied through a ROOT_PATH prefix.
export default defineConfig(({ command }) => ({
  plugins: [react()],
  base: command === "build" ? "./" : "/",
  server: {
    port: 5173,
    strictPort: true,
    hmr: { clientPort: 8080 },
  },
}));
