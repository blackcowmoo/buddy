// Mirrors httpserver.recordingsListHandler's response shape
// (apps/server/internal/httpserver/recordings.go).
export interface Recording {
  id: string;
  createdAt: number; // unix seconds
  durationMs: number;
  sizeBytes: number;
}

// Fetches the caller's own archived recordings, most recent first. Relative
// URL, same reasoning as lib/me.ts's fetchMe: resolves against the current
// page, so this works whether the app is mounted at "/" or under a
// ROOT_PATH prefix. Returns null on any failure — network error, non-200
// (including 503 when recording storage isn't configured), or bad JSON — so
// the caller can show an empty/error state instead of throwing.
export async function fetchRecordings(): Promise<Recording[] | null> {
  try {
    const res = await fetch("api/recordings");
    if (!res.ok) return null;
    return (await res.json()) as Recording[];
  } catch {
    return null;
  }
}

// Same-origin relative URL for playing back one recording (see
// httpserver.recordingAudioHandler) — safe to drop straight into an
// <audio src>.
export function recordingAudioURL(id: string): string {
  return `api/recordings/${encodeURIComponent(id)}/audio`;
}

// Deletes one archived recording. Returns whether the request succeeded, so
// the caller can decide what to do on failure (e.g. leave the row in the
// list) instead of assuming success.
export async function deleteRecording(id: string): Promise<boolean> {
  try {
    const res = await fetch(`api/recordings/${encodeURIComponent(id)}`, { method: "DELETE" });
    return res.ok;
  } catch {
    return false;
  }
}
