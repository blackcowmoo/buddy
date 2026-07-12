import { defineConfig } from "vitest/config";

// Node >=26 defines a native `localStorage` global by default (a no-op
// getter, since it needs --localstorage-file to actually work). Vitest's
// jsdom environment only overrides globals that Node doesn't already
// define, so that no-op shadows jsdom's real Storage implementation and
// every jsdom-environment test touching localStorage sees `undefined`.
// --no-webstorage removes Node's global so Vitest can install jsdom's.
// Guarded by version because the flag doesn't exist before Node 26 and
// Node then refuses to start at all.
const nodeMajor = Number(process.versions.node.split(".")[0]);
const execArgv = nodeMajor >= 26 ? ["--no-webstorage"] : [];

export default defineConfig({
  test: {
    environment: "node",
    execArgv,
    coverage: {
      provider: "v8",
      reporter: ["text", "json-summary", "lcov"],
      include: ["src/**/*.{ts,tsx}"],
      exclude: ["src/**/*.test.{ts,tsx}", "src/main.tsx", "src/vite-env.d.ts"],
    },
  },
});
