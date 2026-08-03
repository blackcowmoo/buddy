import { StrictMode } from "react";
import { createRoot } from "react-dom/client";
import { App } from "./App";
import { Recordings } from "./pages/Recordings";
import { WordReview } from "./pages/WordReview";
import { WordMatch } from "./pages/WordMatch";
import { InstantSessions } from "./pages/InstantSessions";
import { currentPage } from "./lib/route";
import "./styles.css";

const page = currentPage(window.location.pathname);

function renderPage() {
  if (page === "recordings") return <Recordings />;
  if (page === "words") return <WordReview />;
  if (page === "match") return <WordMatch />;
  if (page === "instant") return <InstantSessions />;
  return <App />;
}

createRoot(document.getElementById("root")!).render(<StrictMode>{renderPage()}</StrictMode>);
