/** @vitest-environment jsdom */
import "@testing-library/jest-dom/vitest";
import { cleanup, render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";
import { SessionList } from "./SessionList";

afterEach(cleanup);

function props() {
  return {
    sessions: [], loading: false, loadError: false, actionError: null,
    openingId: null, deletingId: null,
    onOpen: vi.fn(), onDelete: vi.fn(), onRetry: vi.fn(), onDismissError: vi.fn(),
  };
}

describe("guided home", () => {
  it("explains both conversation modes and keeps their actions distinct", async () => {
    const actions = props();
    const user = userEvent.setup();
    render(<SessionList {...actions} />);
    expect(screen.getByRole("heading", { name: "오늘도 영어와 가까워져요" })).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "+ 새 대화" })).toHaveAccessibleDescription("Buddy와 자유롭게 이야기해요");
    expect(screen.getByRole("button", { name: /인스턴트 대화/ })).toHaveAccessibleDescription("한 문장으로 가볍게 연습해요");
    await user.click(screen.getByRole("button", { name: "+ 새 대화" }));
    expect(actions.onOpen).toHaveBeenLastCalledWith();
    await user.click(screen.getByRole("button", { name: /인스턴트 대화/ }));
    expect(actions.onOpen).toHaveBeenLastCalledWith(undefined, true);
  });

  it("exposes all learning paths with explanations and preserves deployment prefixes", () => {
    render(<SessionList {...props()} />);
    const nav = screen.getByRole("navigation", { name: "오늘은 무엇을 해 볼까요?" });
    const destinations = [
      ["단어 뉘앙스", "nuance"], ["오늘의 작문", "writing"], ["단어 복습", "words"],
      ["오늘의 아티클", "article"], ["단어 매칭 게임", "match"], ["녹음 목록", "recordings"],
    ];
    expect(within(nav).getAllByRole("link")).toHaveLength(destinations.length);
    for (const [title, path] of destinations) {
      const link = within(nav).getByRole("link", { name: new RegExp(title) });
      expect(link).toHaveAttribute("href", path);
      expect(link.querySelector(".learning-path-description")?.textContent).toBeTruthy();
      for (const base of ["https://buddy.test/", "https://buddy.test/pr/14/"]) {
        expect(new URL(link.getAttribute("href")!, base).href).toBe(`${base}${path}`);
      }
    }
    expect(screen.getByRole("link", { name: "인스턴트 기록" })).toHaveAttribute("href", "instant");
  });

  it("keeps existing conversations available below the learning shortcuts", async () => {
    const actions = props();
    const user = userEvent.setup();
    render(<SessionList {...actions} sessions={[{ id: "s1", title: "My day", createdAt: 1, updatedAt: 2 }]} />);
    expect(screen.queryByText("아직 대화 기록이 없어요")).not.toBeInTheDocument();
    await user.click(screen.getByRole("button", { name: /My day/ }));
    expect(actions.onOpen).toHaveBeenCalledWith("s1");
  });
});
