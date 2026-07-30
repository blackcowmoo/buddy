import { requestOK } from "./fetchJSON";

// Mirrors httpserver.settingsGetHandler/settingsSaveHandler's response shape
// (apps/server/internal/httpserver/server.go). Global to the user (not
// per-session): it's layered onto the chat persona's system prompt when a
// session is created (see pipeline.BuildSystemPrompt).
export interface Settings {
  interlocutorStyle: string;
  // The learner's persistent, LLM-maintained cross-session profile (recurring
  // mistakes, interests, proficiency trend) — folded in from each ended
  // session's study summary (see store.Store.GetLearnerProfile). Read-only:
  // nothing under api/settings writes it, it only rides along on the same
  // fetch so the menu can show it without a second round trip. "" if the
  // learner hasn't ended a session yet.
  learnerProfile: string;
}

// Mirrors maxInterlocutorStyleLen in httpserver/server.go — the server
// rejects longer values with a 400, so the form validates against the same
// bound up front instead of letting the user submit into that silently.
export const MAX_INTERLOCUTOR_STYLE_LEN = 1024;

// Fetches the caller's saved conversation-style preference. Returns null on
// any failure (network error, non-200, bad JSON) rather than falling back to
// an empty style: a transient load failure and "never set one" must stay
// distinguishable, since the caller wires this into a form whose submit
// button would otherwise happily persist that fallback empty string over a
// real saved value.
export async function fetchSettings(): Promise<Settings | null> {
  try {
    const res = await fetch("api/settings");
    if (!res.ok) return null;
    return (await res.json()) as Settings;
  } catch {
    return null;
  }
}

// Saves the caller's conversation-style preference. Takes effect on chat
// sessions created from now on, not any already-open connection (see
// httpserver.settingsSaveHandler). Returns whether the request succeeded.
export async function saveSettings(interlocutorStyle: string): Promise<boolean> {
  return requestOK("api/settings", {
    method: "PUT",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ interlocutorStyle }),
  });
}
