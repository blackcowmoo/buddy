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

  it("uses an opaque surface for popovers in both themes", () => {
    const darkTheme = styles.match(/:root\s*\{([\s\S]*?)\n\}/)?.[1] ?? "";
    const lightTheme = styles.match(/:root\[data-theme="white"\]\s*\{([\s\S]*?)\n\}/)?.[1] ?? "";

    expect(darkTheme).toContain("--overlay: var(--panel);");
    expect(lightTheme).toContain("--overlay: var(--panel);");

    for (const selector of [
      ".menu-panel",
      ".study-panel",
      ".word-lookup-panel",
      ".article-searched-words-panel",
    ]) {
      const escapedSelector = selector.replace(/[.*+?^${}()|[\]\\]/g, "\\$&");
      const rule = styles.match(new RegExp(`${escapedSelector}\\s*\\{([\\s\\S]*?)\\n\\}`))?.[1] ?? "";
      expect(rule).toContain("background: var(--overlay);");
      expect(rule).not.toContain("backdrop-filter");
    }
  });

  it("keeps keyboard focus and reduced-motion treatment in the global UI layer", () => {
    expect(styles).toContain("button:focus-visible");
    expect(styles).toContain("@media (prefers-reduced-motion: reduce)");
  });

  it("centers conversation feedback on the full header instead of its right-hand menu", () => {
    const header = styles.match(/\.topbar\s*\{([\s\S]*?)\n\}/)?.[1] ?? "";
    const panel = styles.match(/\.feedback-panel\s*\{([\s\S]*?)\n\}/)?.[1] ?? "";

    // Fixed positioning skips .menu; layout containment anchors the panel
    // to the centered app header even if its backdrop filter is removed.
    expect(header).toContain("contain: layout;");
    expect(panel).toContain("position: fixed;");
    expect(panel).toContain("left: 50%;");
    expect(panel).toContain("right: auto;");
    expect(panel).toContain("transform: translateX(-50%);");
  });

  it("reserves equal feedback gutters of at least 24px, including landscape safe areas", () => {
    const panel = styles.match(/\.feedback-panel\s*\{([\s\S]*?)\n\}/)?.[1] ?? "";

    expect(panel).toContain("--feedback-inline-margin: max(24px, var(--layout-inline-padding), env(safe-area-inset-left, 0px), env(safe-area-inset-right, 0px));");
    expect(panel).toContain("calc(100% - 2 * var(--feedback-inline-margin))");
    expect(panel).toContain("overflow-wrap: anywhere;");
    expect(panel).toContain("overflow-y: auto;");
  });

  it("keeps tappable article words flat until one is hovered, focused, or selected", () => {
    const articleWord = styles.match(/\.article-word\s*\{([\s\S]*?)\n\}/)?.[1] ?? "";

    expect(articleWord).toContain("background: transparent;");
    expect(articleWord).toContain("border: 0;");
    expect(articleWord).toContain("box-shadow: none;");
    expect(articleWord).toContain("filter: none;");
    expect(articleWord).toContain("transform: none;");
    expect(styles).toContain(".article-word.selected");
    expect(styles).toContain(".article-word:hover:not(:disabled)");
  });
});
