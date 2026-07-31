// Caches the composer's in-progress, not-yet-sent text per chat room, so
// opening a side panel (or switching rooms and back) never silently drops
// what the learner was mid-typing. Pure client convenience — nothing here is
// synced to the server — so it lives in localStorage like every other client
// preference (see storedValue.ts), just with an expiry added: unlike a
// setting, a stale draft for a room the learner hasn't opened in a week is
// more likely to be confusing leftover text than something worth restoring.
import { readStored, writeStored } from "./storedValue";

const DRAFT_TTL_MS = 7 * 24 * 60 * 60 * 1000; // ~1 week, same duration as DefaultCacheTTL server-side (internal/llm/cached.go)

function draftKey(sessionId: string): string {
  return `buddy.chat.draft.${sessionId}`;
}

interface StoredDraft {
  text: string;
  expiresAt: number;
}

function isStoredDraft(v: unknown): v is StoredDraft {
  return (
    typeof v === "object" &&
    v !== null &&
    typeof (v as StoredDraft).text === "string" &&
    typeof (v as StoredDraft).expiresAt === "number"
  );
}

/** Reads sessionId's cached draft, or "" if there is none or it has expired. */
export function loadDraft(sessionId: string): string {
  return readStored(
    draftKey(sessionId),
    (raw) => {
      const parsed: unknown = JSON.parse(raw);
      if (!isStoredDraft(parsed) || Date.now() > parsed.expiresAt) return undefined;
      return parsed.text;
    },
    "",
  );
}

/** Persists sessionId's in-progress draft in real time, or clears it once emptied. */
export function saveDraft(sessionId: string, text: string): void {
  if (text === "") {
    clearDraft(sessionId);
    return;
  }
  writeStored(draftKey(sessionId), JSON.stringify({ text, expiresAt: Date.now() + DRAFT_TTL_MS } satisfies StoredDraft));
}

/** Drops sessionId's cached draft — called once its text is actually sent. */
export function clearDraft(sessionId: string): void {
  try {
    localStorage.removeItem(draftKey(sessionId));
  } catch {
    // Storage unavailable — nothing to clear.
  }
}
