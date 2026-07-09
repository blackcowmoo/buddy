import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";

// Single origin: the browser always talks to the Go server on :8080, which
// proxies HTML/HMR here in dev. So the HMR socket must report back through
// :8080, not Vite's own port.
export default defineConfig({
  plugins: [react()],
  server: {
    port: 5173,
    strictPort: true,
    hmr: { clientPort: 8080 },
  },
});
