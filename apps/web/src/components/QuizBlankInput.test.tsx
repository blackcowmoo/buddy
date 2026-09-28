/** @vitest-environment jsdom */
import "@testing-library/jest-dom/vitest";
import { createRef, useState } from "react";
import { cleanup, render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";
import { QuizBlankInput } from "./QuizBlankInput";

afterEach(cleanup);

describe("QuizBlankInput", () => {
  it.each([
    ["", "3ch"],
    ["hello", "6ch"],
    ["a sufficiently long answer", "16ch"],
  ])("sizes the blank from the typed answer %j", (value, width) => {
    render(<QuizBlankInput value={value} checked={false} correct={false} readOnly />);
    expect(screen.getByRole("textbox")).toHaveStyle({ width });
  });

  it.each([true, false])("reveals correctness only after checking (correct: %s)", (correct) => {
    const { rerender } = render(<QuizBlankInput value="answer" checked={false} correct={correct} readOnly />);
    const input = screen.getByRole("textbox");
    expect(input).toHaveClass("quiz-blank-input", { exact: true });
    expect(input).toBeEnabled();

    rerender(<QuizBlankInput value="answer" checked correct={correct} readOnly />);
    expect(input).toHaveClass(correct ? "correct" : "incorrect");
    expect(input).toBeDisabled();
  });

  it("disables editing during an asynchronous check without revealing the result", () => {
    render(<QuizBlankInput value="answer" checked={false} correct={false} disabled />);
    expect(screen.getByRole("textbox")).toBeDisabled();
    expect(screen.getByRole("textbox")).toHaveClass("quiz-blank-input", { exact: true });
  });

  it("preserves focus, labels, input limits, and keyboard handling", async () => {
    const ref = createRef<HTMLInputElement>();
    const submit = vi.fn();
    function Quiz() {
      const [answer, setAnswer] = useState("");
      return <QuizBlankInput
        ref={ref}
        autoFocus
        aria-label="정답 입력"
        maxLength={5}
        value={answer}
        checked={false}
        correct={false}
        onChange={(event) => setAnswer(event.target.value)}
        onKeyDown={(event) => { if (event.key === "Enter") submit(event.currentTarget.value); }}
      />;
    }
    const user = userEvent.setup();
    render(<Quiz />);
    const input = screen.getByRole("textbox", { name: "정답 입력" });
    expect(ref.current).toBe(input);
    expect(input).toHaveFocus();
    await user.keyboard("hello!{Enter}");
    expect(input).toHaveValue("hello");
    expect(submit).toHaveBeenCalledExactlyOnceWith("hello");
  });
});
