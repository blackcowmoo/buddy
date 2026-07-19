import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";
import { viteStaticCopy } from "vite-plugin-static-copy";

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
  plugins: [
    react(),
    // recorder.ts's VAD-based silence trimming (@ricky0123/vad-web) fetches
    // its ONNX model and onnxruntime-web's wasm runtime as plain static
    // files at request time, the same way pcm-worklet.js already is — so
    // they need to land next to it at the site root instead of going
    // through Vite's JS bundling.
    viteStaticCopy({
      targets: [
        {
          src: "node_modules/@ricky0123/vad-web/dist/silero_vad_legacy.onnx",
          dest: ".",
          rename: { stripBase: true },
        },
        {
          src: "node_modules/onnxruntime-web/dist/*.{wasm,mjs}",
          dest: ".",
          rename: { stripBase: true },
        },
      ],
    }),
  ],
  base: command === "build" ? "./" : "/",
  server: {
    port: 5173,
    strictPort: true,
    hmr: { clientPort: 8080 },
  },
}));
