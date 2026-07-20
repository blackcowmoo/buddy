// Gives the room list and an open chat room their own browser history entry,
// via the URL hash (never sent to the server, so it needs no server-side
// route — see lib/route.ts). Without this, switching between them was pure
// React state with no history entry at all, so back/swipe-back had nothing
// of ours to undo and exited the app instead. Kept as its own module (rather
// than calling window.history directly from App.tsx) so App's tests can mock
// it instead of fighting jsdom's location/history plumbing.
export type RoomHistoryState = { view: "list" } | { view: "chat"; id: string | null };

// A brand-new room has no id until the server mints one on "ready", but the
// history entry still needs to exist the instant the learner opens it (so an
// immediate back/swipe returns to the list) — "new" is that placeholder.
function hashFor(state: RoomHistoryState): string {
  if (state.view !== "chat") return "";
  return `#chat/${state.id ? encodeURIComponent(state.id) : "new"}`;
}

function urlFor(state: RoomHistoryState): string {
  return window.location.pathname + window.location.search + hashFor(state);
}

export function parseRoomHash(hash: string): RoomHistoryState {
  const m = /^#chat\/(.+)$/.exec(hash);
  if (!m) return { view: "list" };
  return { view: "chat", id: m[1] === "new" ? null : decodeURIComponent(m[1]) };
}

export function currentRoomHistoryState(): RoomHistoryState {
  return (window.history.state as RoomHistoryState | null) ?? parseRoomHash(window.location.hash);
}

export function pushRoomState(state: RoomHistoryState): void {
  window.history.pushState(state, "", urlFor(state));
}

// Swaps the current entry in place (e.g. upgrading a pending "new" room to
// its real id) without adding a new one to the stack.
export function replaceRoomState(state: RoomHistoryState): void {
  window.history.replaceState(state, "", urlFor(state));
}

export function goBack(): void {
  window.history.back();
}

/** Reports the resolved room state on browser back/forward (incl. swipe). */
export function onRoomPopState(handler: (state: RoomHistoryState) => void): () => void {
  const listener = (e: PopStateEvent) => {
    handler((e.state as RoomHistoryState | null) ?? parseRoomHash(window.location.hash));
  };
  window.addEventListener("popstate", listener);
  return () => window.removeEventListener("popstate", listener);
}
