// Picks which top-level page to render, based on the current URL path. There
// is no router library here (this app has exactly two pages) — just a plain
// suffix check, relative to wherever the app happens to be mounted (root, or
// under a ROOT_PATH prefix like "/pr/14"; see httpserver.withRootPath and
// lib/rootPath.ts). The server's SPA fallback (spaHandlerFS) serves
// index.html for any unmatched path, so this works without a server-side
// route for "/recordings" or "/pr/14/recordings".
export type Page = "chat" | "recordings";

export function currentPage(pathname: string): Page {
  return pathname.replace(/\/+$/, "").endsWith("/recordings") ? "recordings" : "chat";
}
