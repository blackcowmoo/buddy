// Theme preference is a client-only setting: it lives in localStorage and is
// never sent to or read from the server. "system" tracks the OS/browser
// prefers-color-scheme instead of pinning to one palette.
import { readStored, writeStored } from "./storedValue";

export type Theme = "white" | "dark" | "system";

const STORAGE_KEY = "buddy-theme";
const THEMES: readonly Theme[] = ["white", "dark", "system"];

function isTheme(value: string): value is Theme {
  return THEMES.includes(value as Theme);
}

export function getStoredTheme(): Theme {
  return readStored(STORAGE_KEY, (raw) => (isTheme(raw) ? raw : undefined), "system");
}

export function setStoredTheme(theme: Theme): void {
  writeStored(STORAGE_KEY, theme);
}

// The concrete palette a theme choice resolves to once "system" is settled
// against the current OS/browser preference.
export type ResolvedTheme = "white" | "dark";

export function resolveTheme(theme: Theme): ResolvedTheme {
  if (theme !== "system") return theme;
  return window.matchMedia("(prefers-color-scheme: light)").matches ? "white" : "dark";
}

// Applies the resolved palette to the document so index.html's [data-theme]
// CSS rules and mobile browser chrome (theme-color / color-scheme) match.
export function applyTheme(theme: Theme): void {
  const resolved = resolveTheme(theme);
  document.documentElement.setAttribute("data-theme", resolved);

  const themeColor = document.querySelector('meta[name="theme-color"]');
  themeColor?.setAttribute("content", resolved === "white" ? "#ffffff" : "#0f1115");

  const colorScheme = document.querySelector('meta[name="color-scheme"]');
  colorScheme?.setAttribute("content", resolved === "white" ? "light" : "dark");
}
