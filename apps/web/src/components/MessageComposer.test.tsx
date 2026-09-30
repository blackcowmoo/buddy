/** @vitest-environment jsdom */
import "@testing-library/jest-dom/vitest";
import type { ComponentProps } from "react";
import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";
import { MessageComposer } from "./MessageComposer";

vi.mock("./WordSearchControl", () => ({
  WordSearchControl: () => <button type="button">모르는 단어 찾기</button>,
}));

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
});

function composerProps(overrides: Partial<ComponentProps<typeof MessageComposer>> = {}) {
  return {
    ended: false,
    quickMode: false,
    quickSent: false,
    mic: false,
    transcribing: false,
    text: "Hello Buddy",
    voiceDraft: false,
    onToggleMic: vi.fn(),
    onSend: vi.fn(),
    onTextChange: vi.fn(),
    onDiscardVoiceDraft: vi.fn(),
    ...overrides,
  };
}

describe("MessageComposer", () => {
  it.each(["", "   ", "\n\t"])("disables sending a whitespace-only draft %j", async (text) => {
    const props = composerProps({ text });
    const user = userEvent.setup();
    render(<MessageComposer {...props} />);

    const send = screen.getByRole("button", { name: "메시지 보내기" });
    expect(send).toBeDisabled();
    await user.click(send);
    expect(props.onSend).not.toHaveBeenCalled();
  });

  it("sends from the form and send button without navigating", async () => {
    const props = composerProps();
    const user = userEvent.setup();
    render(<MessageComposer {...props} />);

    const form = screen.getByRole("textbox", { name: "영어 메시지" }).closest("form")!;
    expect(fireEvent.submit(form)).toBe(false);
    expect(props.onSend).toHaveBeenCalledExactlyOnceWith();

    await user.click(screen.getByRole("button", { name: "메시지 보내기" }));
    expect(props.onSend).toHaveBeenCalledTimes(2);
  });

  it("sends on Enter and prevents inserting a newline", () => {
    const props = composerProps();
    render(<MessageComposer {...props} />);

    const input = screen.getByRole("textbox", { name: "영어 메시지" });
    expect(fireEvent.keyDown(input, { key: "Enter", code: "Enter" })).toBe(false);
    expect(props.onSend).toHaveBeenCalledExactlyOnceWith();
  });

  it.each([
    ["Shift+Enter", { key: "Enter", code: "Enter", shiftKey: true }],
    ["active IME composition", { key: "Enter", code: "Enter", isComposing: true }],
    ["legacy IME key code", { key: "Enter", code: "Enter", keyCode: 229 }],
    ["ordinary typing", { key: "a", code: "KeyA" }],
  ])("preserves native keyboard behavior for %s", (_label, event) => {
    const props = composerProps();
    render(<MessageComposer {...props} />);

    const input = screen.getByRole("textbox", { name: "영어 메시지" });
    expect(fireEvent.keyDown(input, event)).toBe(true);
    expect(props.onSend).not.toHaveBeenCalled();
  });

  it("reports edits and resizes when the controlled draft grows or shrinks", () => {
    const props = composerProps({ text: "Hi" });
    const { rerender } = render(<MessageComposer {...props} />);
    const input = screen.getByRole<HTMLTextAreaElement>("textbox", { name: "영어 메시지" });
    // jsdom has no layout; supply deterministic content heights for both sizes.
    Object.defineProperty(input, "scrollHeight", {
      get: () => input.value.includes("\n") ? 72 : 24,
    });

    const multiline = "First line\nSecond line\nThird line";
    fireEvent.change(input, { target: { value: multiline } });
    expect(props.onTextChange).toHaveBeenCalledExactlyOnceWith(multiline);

    rerender(<MessageComposer {...props} text={multiline} />);
    expect(input).toHaveStyle({ height: "72px" });
    rerender(<MessageComposer {...props} text="Short" />);
    expect(input).toHaveStyle({ height: "24px" });
  });

  it.each([{ ended: true }, { quickMode: true, quickSent: true }])(
    "sizes an unchanged draft when editing becomes available after %o",
    (state) => {
      vi.spyOn(HTMLTextAreaElement.prototype, "scrollHeight", "get").mockReturnValue(64);
      const props = composerProps({ text: "A restored draft\nwith another line" });
      const { rerender } = render(<MessageComposer {...props} {...state} />);
      expect(screen.queryByRole("textbox", { name: "영어 메시지" })).not.toBeInTheDocument();

      rerender(<MessageComposer {...props} />);
      const input = screen.getByRole("textbox", { name: "영어 메시지" });
      expect(input).toHaveValue(props.text);
      expect(input).toHaveStyle({ height: "64px" });
    },
  );

  it.each([
    [{ ended: true }, "이 대화는 종료되어 더 이상 메시지를 보낼 수 없어요."],
    [{ ended: true, quickMode: true }, "인스턴트 대화를 완료했어요!"],
    [{ quickMode: true, quickSent: true }, "답변을 기다리는 중이에요…"],
  ])("shows the room status without editing controls for %o", (state, message) => {
    render(<MessageComposer {...composerProps(state)} />);

    expect(screen.getByRole("status")).toHaveTextContent(message);
    expect(screen.queryByRole("textbox", { name: "영어 메시지" })).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "메시지 보내기" })).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "모르는 단어 찾기" })).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "음성으로 말하기" })).not.toBeInTheDocument();
  });
});
