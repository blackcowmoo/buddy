// Mirrors httpserver.meHandler's response shape (apps/server/internal/httpserver/server.go).
export interface Identity {
  identityMode: string;
  id: string;
}

// Fetches the caller's resolved identity from the server. Relative URL: it
// resolves against the current page, so this still hits the right server
// whether the app is mounted at "/" or under a ROOT_PATH prefix like
// "/pr/14". Returns null on any failure — network error, non-200, or bad
// JSON — so the caller can fall back to an anonymous label.
export async function fetchMe(): Promise<Identity | null> {
  try {
    const res = await fetch("api/me");
    if (!res.ok) return null;
    return (await res.json()) as Identity;
  } catch {
    return null;
  }
}
