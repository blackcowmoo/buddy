// Shared shape behind every localStorage-backed client setting (theme,
// playback speeds, …): read-and-validate with a fallback, or swallow a
// failed write. Wrapped in try/catch on both sides since localStorage can
// throw (private browsing, quota) even when the key itself is well-formed.

export function readStored<T>(key: string, parse: (raw: string) => T | undefined, fallback: T): T {
  let raw: string | null;
  try {
    raw = localStorage.getItem(key);
  } catch {
    return fallback;
  }
  if (raw === null) return fallback;
  try {
    return parse(raw) ?? fallback;
  } catch {
    return fallback;
  }
}

export function writeStored(key: string, value: string): void {
  try {
    localStorage.setItem(key, value);
  } catch {
    // Storage unavailable — the setting just won't survive a reload;
    // nothing else depends on it.
  }
}
