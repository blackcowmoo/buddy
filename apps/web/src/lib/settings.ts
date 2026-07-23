import { fetchJSON, requestOK } from "./fetchJSON";

// Mirrors httpserver.settingsGetHandler/settingsSaveHandler's response shape
// (apps/server/internal/httpserver/server.go). Global to the user (not
// per-session): it's layered onto the chat persona's system prompt when a
// session is created (see pipeline.BuildSystemPrompt).
export interface Settings {
  interlocutorStyle: string;
}

// Fetches the caller's saved conversation-style preference. Returns ""
// on any failure so the settings UI can render an empty field instead of
// throwing.
export async function fetchSettings(): Promise<Settings> {
  return fetchJSON<Settings>("api/settings", { interlocutorStyle: "" });
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
