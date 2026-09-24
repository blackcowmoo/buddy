/** @vitest-environment jsdom */
import "@testing-library/jest-dom/vitest";
import { cleanup, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it } from "vitest";
import { LearningMenuItems } from "./LearningMenuItems";

afterEach(cleanup);

describe("LearningMenuItems", () => {
  it("keeps every learning destination in one relative-link list", () => {
    render(<div role="menu"><LearningMenuItems wordDueCount={3} nuanceDueCount={4} /></div>);
    expect(screen.getAllByRole("menuitem")).toHaveLength(7);
    expect(screen.getByRole("menuitem", { name: /단어 복습/ })).toHaveTextContent("3");
    expect(screen.getByRole("menuitem", { name: /단어 뉘앙스/ })).toHaveTextContent("4");
    expect(screen.getByRole("menuitem", { name: /오늘의 작문/ })).toHaveAttribute("href", "writing");
  });

  it("supports the callback form used by the main screen menu", () => {
    const selected: string[] = [];
    render(<div role="menu"><LearningMenuItems onSelect={(key) => selected.push(key)} /></div>);
    screen.getByRole("menuitem", { name: /단어 뉘앙스/ }).click();
    expect(selected).toEqual(["nuance"]);
  });
});
