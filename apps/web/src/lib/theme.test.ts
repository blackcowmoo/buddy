/**
 * @vitest-environment jsdom
 */
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { applyTheme, getStoredTheme, onSystemThemeChange, resolveTheme, setStoredTheme } from "./theme";

function stubMatchMedia(prefersLight: boolean) {
  vi.stubGlobal(
    "matchMedia",
    vi.fn().mockImplementation((query: string) => ({
      matches: query === "(prefers-color-scheme: light)" && prefersLight,
      media: query,
      addEventListener: vi.fn(),
      removeEventListener: vi.fn(),
    })),
  );
}

beforeEach(() => {
  localStorage.clear();
  document.documentElement.removeAttribute("data-theme");
  document.head.innerHTML = `
    <meta name="theme-color" content="#0f1115" />
    <meta name="color-scheme" content="dark light" />
  `;
});

afterEach(() => {
  vi.unstubAllGlobals();
});

describe("getStoredTheme", () => {
  it("defaults to system when nothing is stored", () => {
    expect(getStoredTheme()).toBe("system");
  });

  it("returns a previously stored theme", () => {
    localStorage.setItem("buddy-theme", "white");
    expect(getStoredTheme()).toBe("white");
  });

  it("falls back to system for a corrupted/unknown value", () => {
    localStorage.setItem("buddy-theme", "blue");
    expect(getStoredTheme()).toBe("system");
  });
});

describe("setStoredTheme", () => {
  it("persists the theme to localStorage only", () => {
    setStoredTheme("dark");
    expect(localStorage.getItem("buddy-theme")).toBe("dark");
  });
});

describe("resolveTheme", () => {
  it("passes explicit choices through unchanged", () => {
    expect(resolveTheme("white")).toBe("white");
    expect(resolveTheme("dark")).toBe("dark");
  });

  it("resolves system to white when the OS prefers light", () => {
    stubMatchMedia(true);
    expect(resolveTheme("system")).toBe("white");
  });

  it("resolves system to dark when the OS does not prefer light", () => {
    stubMatchMedia(false);
    expect(resolveTheme("system")).toBe("dark");
  });
});

describe("applyTheme", () => {
  it("sets data-theme and mobile chrome meta tags for an explicit theme", () => {
    stubMatchMedia(false);
    applyTheme("white");
    expect(document.documentElement.getAttribute("data-theme")).toBe("white");
    expect(document.querySelector('meta[name="theme-color"]')?.getAttribute("content")).toBe(
      "#ffffff",
    );
    expect(document.querySelector('meta[name="color-scheme"]')?.getAttribute("content")).toBe(
      "light",
    );
  });

  it("resolves system before applying", () => {
    stubMatchMedia(true);
    applyTheme("system");
    expect(document.documentElement.getAttribute("data-theme")).toBe("white");
  });
});

describe("onSystemThemeChange", () => {
  it("subscribes the handler to the system-theme media query's change event", () => {
    stubMatchMedia(true);
    const handler = vi.fn();
    onSystemThemeChange(handler);
    const mql = vi.mocked(window.matchMedia).mock.results.at(-1)!.value;
    expect(mql.addEventListener).toHaveBeenCalledWith("change", handler);
  });

  it("returns an unsubscribe function that removes the handler", () => {
    stubMatchMedia(true);
    const handler = vi.fn();
    const unsubscribe = onSystemThemeChange(handler);
    const mql = vi.mocked(window.matchMedia).mock.results.at(-1)!.value;
    unsubscribe();
    expect(mql.removeEventListener).toHaveBeenCalledWith("change", handler);
  });
});
