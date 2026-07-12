// Relative URLs resolve against the current page, so callers work whether
// the app is mounted at "/" or under a ROOT_PATH prefix like "/pr/14".
// Returns fallback on any failure — network error, non-200, or bad JSON —
// so callers can degrade (empty list, null, anonymous label) instead of
// throwing.
export async function fetchJSON<T>(url: string, fallback: T): Promise<T> {
  try {
    const res = await fetch(url);
    if (!res.ok) return fallback;
    return (await res.json()) as T;
  } catch {
    return fallback;
  }
}
