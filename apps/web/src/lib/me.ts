import { fetchJSON } from "./fetchJSON";

// Mirrors httpserver.meHandler's response shape (apps/server/internal/httpserver/server.go).
export interface Identity {
  identityMode: string;
  id: string;
}

// Fetches the caller's resolved identity from the server. Returns null on
// any failure so the caller can fall back to an anonymous label.
export async function fetchMe(): Promise<Identity | null> {
  return fetchJSON<Identity | null>("api/me", null);
}
