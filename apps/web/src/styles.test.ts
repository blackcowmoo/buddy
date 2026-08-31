import { readFileSync } from "node:fs";
import { describe, expect, it } from "vitest";

const styles = readFileSync(new URL("./styles.css", import.meta.url), "utf8");

describe("visual design system", () => {
  it("defines the shared surface, accent, elevation, and focus tokens for both themes", () => {
    const darkTheme = styles.match(/:root\s*\{([\s\S]*?)\n\}/)?.[1] ?? "";
    const lightTheme = styles.match(/:root\[data-theme="white"\]\s*\{([\s\S]*?)\n\}/)?.[1] ?? "";
    const requiredTokens = [
      "--bg-deep",
      "--panel-3",
      "--overlay",
      "--accent-soft",
      "--accent-border",
      "--user-strong",
      "--shadow-xs",
      "--shadow-lg",
      "--focus-ring",
    ];

    for (const token of requiredTokens) {
      expect(darkTheme).toContain(`${token}:`);
      expect(lightTheme).toContain(`${token}:`);
    }
  });

  it("keeps keyboard focus and reduced-motion treatment in the global UI layer", () => {
    expect(styles).toContain("button:focus-visible");
    expect(styles).toContain("@media (prefers-reduced-motion: reduce)");
  });
});
