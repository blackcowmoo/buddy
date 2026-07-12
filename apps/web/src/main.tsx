import { StrictMode } from "react";
import { createRoot } from "react-dom/client";
import { App } from "./App";
import { Recordings } from "./pages/Recordings";
import { currentPage } from "./lib/route";
import "./styles.css";

const page = currentPage(window.location.pathname);

createRoot(document.getElementById("root")!).render(
  <StrictMode>{page === "recordings" ? <Recordings /> : <App />}</StrictMode>,
);
