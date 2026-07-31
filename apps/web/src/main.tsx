import { StrictMode } from "react";
import { createRoot } from "react-dom/client";
import { App } from "./App";
import { Recordings } from "./pages/Recordings";
import { WordReview } from "./pages/WordReview";
import { currentPage } from "./lib/route";
import "./styles.css";

const page = currentPage(window.location.pathname);

function renderPage() {
  if (page === "recordings") return <Recordings />;
  if (page === "words") return <WordReview />;
  return <App />;
}

createRoot(document.getElementById("root")!).render(<StrictMode>{renderPage()}</StrictMode>);
