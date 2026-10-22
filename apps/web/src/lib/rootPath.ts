// Derives the path to navigate to when the user asks to jump to a different
// PR-preview deployment, matching the ROOT_PATH convention the Go server
// understands (see httpserver.withRootPath): a bare PR number maps to
// "/pr/<n>/", and an empty input means "back to the root deployment".
// Returns null for anything else so the caller can show an error instead of
// navigating somewhere nonsensical.
export function prPath(input: string): string | null {
  const trimmed = input.trim();
  if (trimmed === "") return "/";
  if (!/^\d+$/.test(trimmed)) return null;
  return `/pr/${trimmed}/`;
}

// The inverse direction: which PR-preview deployment the app is currently
// served from. The mount prefix is visible in the page URL ("/pr/14/...",
// see httpserver.withRootPath), so a leading "/pr/<n>" segment is the PR
// number being previewed; anything else (the root deployment) returns null.
export function currentPrNumber(pathname: string): string | null {
  const match = /^\/pr\/(\d+)(?:\/|$)/.exec(pathname);
  return match ? match[1] : null;
}
