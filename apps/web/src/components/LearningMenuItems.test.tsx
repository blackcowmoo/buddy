/** @vitest-environment jsdom */
import "@testing-library/jest-dom/vitest";
import { cleanup, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it } from "vitest";
import { LearningMenuItems } from "./LearningMenuItems";

const initialURL = window.location.href;

afterEach(() => {
  cleanup();
  window.history.replaceState(null, "", initialURL);
});

describe("LearningMenuItems", () => {
  it("keeps every learning destination in one relative-link list", () => {
    render(<div role="menu"><LearningMenuItems wordDueCount={3} nuanceDueCount={4} /></div>);
    expect(screen.getAllByRole("menuitem")).toHaveLength(7);
    expect(screen.getByRole("menuitem", { name: /단어 복습/ })).toHaveTextContent("3");
    expect(screen.getByRole("menuitem", { name: /단어 뉘앙스/ })).toHaveTextContent("4");
    expect(screen.getByRole("menuitem", { name: /오늘의 작문/ })).toHaveAttribute("href", "writing");
  });

  it.each(["/", "/words", "/pr/14/", "/pr/14/words"])("keeps navigation within the deployment at %s", (path) => {
    window.history.replaceState(null, "", path);
    render(<div role="menu"><LearningMenuItems /></div>);
    const prefix = path.startsWith("/pr/14/") ? "/pr/14/" : "/";
    const destinations = ["recordings", "instant", "words", "match", "nuance", "article", "writing"];

    expect(screen.getAllByRole<HTMLAnchorElement>("menuitem").map((link) => link.href)).toEqual(
      destinations.map((destination) => `${window.location.origin}${prefix}${destination}`),
    );
    expect(screen.getByRole("menuitem", { name: "단어 복습" })).toBeInTheDocument();
    expect(screen.getByRole("menuitem", { name: "단어 뉘앙스" })).toBeInTheDocument();
  });

  it.each([
    ["/words", "단어 복습"],
    ["/pr/14/article", "오늘의 아티클"],
    ["/nuance-review", "단어 뉘앙스"],
    ["/pr/14/nuance-review", "단어 뉘앙스"],
  ])("identifies the current learning destination at %s", (path, label) => {
    window.history.replaceState(null, "", path);
    render(<div role="menu"><LearningMenuItems /></div>);

    expect(screen.getByRole("menuitem", { name: label })).toHaveAttribute("aria-current", "page");
    expect(screen.getAllByRole("menuitem", { current: "page" })).toHaveLength(1);
  });

  it("leaves all learning destinations unselected on the home page", () => {
    window.history.replaceState(null, "", "/pr/14/");
    render(<div role="menu"><LearningMenuItems /></div>);
    expect(screen.queryByRole("menuitem", { current: "page" })).not.toBeInTheDocument();
  });
});
