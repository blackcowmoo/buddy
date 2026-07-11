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
