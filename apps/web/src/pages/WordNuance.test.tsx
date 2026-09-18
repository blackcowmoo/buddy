/** @vitest-environment jsdom */
import "@testing-library/jest-dom/vitest";
import { cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { WordNuance } from "./WordNuance";
import { loadNuanceAnswers, nuanceLessons, saveNuanceAnswers } from "../lib/nuance";

beforeEach(() => {
  localStorage.clear();
  vi.spyOn(Math, "random").mockReturnValue(0);
});
afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
  localStorage.clear();
});

function openLesson(id = "price") {
  const lesson = nuanceLessons.find((item) => item.id === id)!;
  fireEvent.click(screen.getByRole("button", { name: new RegExp(lesson.words[0].word) }));
  return lesson;
}

function practice(id = "price") {
  const lesson = openLesson(id);
  fireEvent.click(screen.getByRole("button", { name: "상황 연습" }));
  return lesson;
}

function questionAt(index: number, lesson = nuanceLessons[0]) {
  return within(screen.getByRole("region", { name: lesson.questions[index].context }));
}

describe("WordNuance", () => {
  it("shows the curriculum, browser storage scope, and a relative back link", () => {
    render(<WordNuance />);
    expect(screen.getByRole("link", { name: "대화로 돌아가기" })).toHaveAttribute("href", ".");
    expect(screen.getByText("6개 묶음 · 맞힌 문맥 0/12")).toBeInTheDocument();
    expect(screen.getByText(/다른 기기와는 동기화되지 않아요/)).toBeInTheDocument();
    for (const lesson of nuanceLessons) {
      expect(screen.getByRole("button", { name: new RegExp(lesson.words[0].word) })).toBeInTheDocument();
    }
  });

  it("filters by Korean meaning or English word without case or surrounding whitespace sensitivity", () => {
    render(<WordNuance />);
    const input = screen.getByRole("searchbox", { name: "비교 묶음 찾기" });
    fireEvent.change(input, { target: { value: "값이 싼" } });
    expect(screen.getByRole("button", { name: /cheap/ })).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: /curious/ })).not.toBeInTheDocument();
    fireEvent.change(input, { target: { value: " CURIOUS " } });
    expect(screen.getByRole("button", { name: /curious/ })).toBeInTheDocument();
    fireEvent.change(input, { target: { value: "없는 단어" } });
    expect(screen.getByRole("status")).toHaveTextContent("일치하는 비교 묶음이 없어요");
  });

  it("explains both words with translated examples, context caveats, and dictionary links", () => {
    render(<WordNuance />);
    const lesson = openLesson();
    for (const word of lesson.words) {
      expect(screen.getByText(word.example)).toBeInTheDocument();
      expect(screen.getByText(word.translation)).toBeInTheDocument();
      expect(screen.getByRole("link", { name: `${word.word} 사전 보기 ↗` })).toHaveAttribute("href", `https://dictionary.cambridge.org/dictionary/english/${word.word}`);
    }
    expect(screen.getByText(lesson.caveat)).toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: "상황에 맞게 골라 보기" }));
    expect(screen.getAllByRole("region")).toHaveLength(2);
  });

  it("withholds translations and explanations until answering, then locks the answer and explains alternatives", () => {
    render(<WordNuance />);
    const lesson = practice();
    const q = lesson.questions[0];
    expect(screen.queryByText(q.translation)).not.toBeInTheDocument();
    expect(screen.queryByText(q.explanation)).not.toBeInTheDocument();
    fireEvent.click(questionAt(0).getByRole("button", { name: "inexpensive" }));
    expect(questionAt(0).getByRole("status")).toHaveTextContent("이 상황에서는 다른 표현이 더 잘 맞아요");
    expect(questionAt(0).getByText(q.translation)).toBeInTheDocument();
    expect(questionAt(0).getByText(q.explanation)).toBeInTheDocument();
    expect(questionAt(0).getByRole("button", { name: "cheap" })).toBeDisabled();
    expect(loadNuanceAnswers()[q.id]).toBe("inexpensive");
    fireEvent.click(questionAt(1).getByRole("button", { name: "inexpensive" }));
    expect(questionAt(1).getByRole("status")).toHaveTextContent("의도에 맞는 표현이에요");
    expect(screen.getByText("6개 묶음 · 맞힌 문맥 1/12")).toBeInTheDocument();
  });

  it("retries only mistakes and preserves correct answers and other lessons", () => {
    saveNuanceAnswers({ "curiosity-science": "curious" });
    render(<WordNuance />);
    practice();
    fireEvent.click(questionAt(0).getByRole("button", { name: "inexpensive" }));
    fireEvent.click(questionAt(1).getByRole("button", { name: "inexpensive" }));
    fireEvent.click(screen.getByRole("button", { name: "틀린 문제 다시 풀기" }));
    expect(questionAt(0).queryByRole("status")).not.toBeInTheDocument();
    expect(questionAt(0).getByRole("button", { name: "cheap" })).toBeEnabled();
    expect(questionAt(1).getByRole("button", { name: "inexpensive" })).toBeDisabled();
    expect(loadNuanceAnswers()).toEqual({ "curiosity-science": "curious", "price-recommend": "inexpensive" });
    fireEvent.click(questionAt(0).getByRole("button", { name: "cheap" }));
    expect(screen.queryByRole("button", { name: "틀린 문제 다시 풀기" })).not.toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: "이 묶음 다시 풀기" }));
    expect(questionAt(1).getByRole("button", { name: "inexpensive" })).toBeEnabled();
    expect(loadNuanceAnswers()).toEqual({ "curiosity-science": "curious" });
  });

  it("restores progress after remount and keeps answers when switching views and lessons", () => {
    const view = render(<WordNuance />);
    practice();
    fireEvent.click(questionAt(0).getByRole("button", { name: "cheap" }));
    fireEvent.click(screen.getByRole("button", { name: "차이 살펴보기" }));
    fireEvent.click(screen.getByRole("button", { name: "상황 연습" }));
    expect(questionAt(0).getByRole("button", { name: "cheap" })).toHaveAttribute("aria-pressed", "true");
    fireEvent.click(screen.getByRole("button", { name: "← 비교 목록" }));
    openLesson("child");
    expect(screen.getByRole("heading", { name: "childlike / childish" })).toBeInTheDocument();
    view.unmount();
    render(<WordNuance />);
    expect(screen.getByText("6개 묶음 · 맞힌 문맥 1/12")).toBeInTheDocument();
    practice();
    expect(questionAt(0).getByRole("button", { name: "cheap" })).toHaveAttribute("aria-pressed", "true");
  });

  it("shuffles answer positions when retrying instead of always putting the correct answer first", () => {
    render(<WordNuance />);
    practice();
    expect(questionAt(0).getAllByRole("button").map((b) => b.textContent)).toEqual(["inexpensive", "cheap"]);
    fireEvent.click(questionAt(0).getByRole("button", { name: "inexpensive" }));
    vi.mocked(Math.random).mockReturnValue(0.99);
    fireEvent.click(screen.getByRole("button", { name: "틀린 문제 다시 풀기" }));
    expect(questionAt(0).getAllByRole("button").map((b) => b.textContent)).toEqual(["cheap", "inexpensive"]);
  });

  it("continues practice but warns when storage is unavailable", () => {
    vi.spyOn(Storage.prototype, "setItem").mockImplementation(() => { throw new Error("quota"); });
    render(<WordNuance />);
    practice();
    fireEvent.click(questionAt(0).getByRole("button", { name: "cheap" }));
    expect(screen.getByRole("alert")).toHaveTextContent("학습 진행을 저장하지 못했어요");
    expect(questionAt(0).getByRole("status")).toHaveTextContent("의도에 맞는 표현이에요");
    vi.mocked(Storage.prototype.setItem).mockRestore();
    fireEvent.click(questionAt(1).getByRole("button", { name: "inexpensive" }));
    expect(screen.queryByRole("alert")).not.toBeInTheDocument();
    expect(Object.keys(loadNuanceAnswers())).toHaveLength(2);
  });

  it.each(nuanceLessons)("can complete every question in $id", (lesson) => {
    render(<WordNuance />);
    practice(lesson.id);
    lesson.questions.forEach((q, i) => {
      fireEvent.click(questionAt(i, lesson).getByRole("button", { name: q.answer }));
      expect(questionAt(i, lesson).getByRole("status")).toHaveTextContent(q.explanation);
    });
    expect(screen.getByText(`풀이 ${lesson.questions.length}/${lesson.questions.length} · 맞힌 문맥 ${lesson.questions.length}/${lesson.questions.length}`)).toBeInTheDocument();
  });
});
