/** @vitest-environment jsdom */
import "@testing-library/jest-dom/vitest";
import { cleanup, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it } from "vitest";
import { LearningPaths } from "./LearningPaths";

const initialURL = window.location.href;

afterEach(() => {
  cleanup();
  window.history.replaceState(null, "", initialURL);
});

describe("LearningPaths", () => {
  it("offers every menu destination with a short description in the same order", () => {
    render(<LearningPaths />);
    expect(screen.getByRole("navigation", { name: "오늘은 무엇을 해 볼까요?" })).toBeInTheDocument();
    expect(screen.getAllByRole("link").map((link) => link.getAttribute("href"))).toEqual([
      "recordings", "instant", "words", "match", "nuance", "article", "writing",
    ]);
    expect(screen.getByRole("link", { name: "인스턴트 대화 목록 짧게 나눈 대화를 이어 봐요" })).toHaveAttribute("href", "instant");
    expect(screen.getByRole("link", { name: "단어 복습 배운 표현을 오래 기억해요" })).toHaveAttribute("href", "words");
  });

  it("keeps cards inside a preview deployment", () => {
    window.history.replaceState(null, "", "/pr/14/");
    render(<LearningPaths />);
    for (const link of screen.getAllByRole<HTMLAnchorElement>("link")) {
      expect(link.href).toBe(`${window.location.origin}/pr/14/${link.getAttribute("href")}`);
    }
  });
});
