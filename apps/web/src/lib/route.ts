// Picks which top-level page to render, based on the current URL path. There
// is no router library here (this app has a small, fixed set of pages) —
// just a plain suffix check, relative to wherever the app happens to be
// mounted (root, or under a ROOT_PATH prefix like "/pr/14"; see
// httpserver.withRootPath and lib/rootPath.ts). The server's SPA fallback
// (spaHandlerFS) serves index.html for any unmatched path, so this works
// without a server-side route for "/recordings", "/words", "/instant", or
// their "/pr/14/..." equivalents.
export type Page = "chat" | "recordings" | "words" | "match" | "instant" | "article" | "writing";

export function currentPage(pathname: string): Page {
  const trimmed = pathname.replace(/\/+$/, "");
  if (trimmed.endsWith("/recordings")) return "recordings";
  if (trimmed.endsWith("/words")) return "words";
  if (trimmed.endsWith("/match")) return "match";
  if (trimmed.endsWith("/instant")) return "instant";
  if (trimmed.endsWith("/article")) return "article";
  if (trimmed.endsWith("/writing")) return "writing";
  return "chat";
}
