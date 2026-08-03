/**
 * @vitest-environment jsdom
 */
import "@testing-library/jest-dom/vitest";
import { cleanup, render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";

vi.mock("../lib/sessions", async () => {
  const actual = await vi.importActual<typeof import("../lib/sessions")>("../lib/sessions");
  return { ...actual, fetchInstantSessions: vi.fn(), deleteSession: vi.fn() };
});

import { InstantSessions } from "./InstantSessions";
import { deleteSession, fetchInstantSessions } from "../lib/sessions";
import { formatDateDivider } from "../lib/time";

afterEach(() => {
  cleanup();
  vi.clearAllMocks();
});

describe("InstantSessions page", () => {
  it("links back to the chat page with a relative href", () => {
    vi.mocked(fetchInstantSessions).mockReturnValue(new Promise(() => {}));
    render(<InstantSessions />);
    expect(screen.getByRole("link", { name: "대화로 돌아가기" })).toHaveAttribute("href", ".");
  });

  it("shows a loading hint before the fetch resolves", () => {
    vi.mocked(fetchInstantSessions).mockReturnValue(new Promise(() => {})); // never resolves
    render(<InstantSessions />);
    expect(screen.getByText("불러오는 중…")).toBeInTheDocument();
  });

  it("shows an empty-state hint when there are no instant sessions", async () => {
    vi.mocked(fetchInstantSessions).mockResolvedValue([]);
    render(<InstantSessions />);
    expect(await screen.findByText("아직 인스턴트 대화가 없습니다.")).toBeInTheDocument();
  });

  // Guards the total-count purpose stated in the page itself — the whole
  // point of this list is knowing how many instant conversations happened
  // and being able to prune bad ones (see the delete tests below).
  it("shows the total count of instant sessions", async () => {
    vi.mocked(fetchInstantSessions).mockResolvedValue([
      { id: "i1", title: "hi", createdAt: 1700000000, updatedAt: 1700000000 },
      { id: "i2", title: "hello", createdAt: 1700000100, updatedAt: 1700000100 },
    ]);
    render(<InstantSessions />);
    expect(await screen.findByText(/지금까지 총 2번 했어요/)).toBeInTheDocument();
  });

  it("shows each session's title", async () => {
    vi.mocked(fetchInstantSessions).mockResolvedValue([
      { id: "i1", title: "how's the weather today", createdAt: 1700000000, updatedAt: 1700000000 },
    ]);
    render(<InstantSessions />);
    expect(await screen.findByText("how's the weather today")).toBeInTheDocument();
  });

  // Guards the date-grouping this page exists for (see its own doc comment):
  // two sessions on the same calendar day share one divider, not one each.
  it("groups sessions from the same day under a single date divider", async () => {
    const morning = 1700000000; // same UTC calendar day as...
    const laterSameDay = morning + 3600; // ...one hour later
    vi.mocked(fetchInstantSessions).mockResolvedValue([
      { id: "i1", title: "first", createdAt: morning, updatedAt: morning },
      { id: "i2", title: "second", createdAt: laterSameDay, updatedAt: laterSameDay },
    ]);
    render(<InstantSessions />);

    await screen.findByText("first");
    expect(screen.getAllByText(formatDateDivider(morning))).toHaveLength(1);
  });

  it("shows a separate divider for sessions on different days", async () => {
    const day1 = 1700000000;
    const day2 = day1 + 86400 * 5; // 5 days later — a different calendar day
    vi.mocked(fetchInstantSessions).mockResolvedValue([
      { id: "i2", title: "newer", createdAt: day2, updatedAt: day2 },
      { id: "i1", title: "older", createdAt: day1, updatedAt: day1 },
    ]);
    render(<InstantSessions />);

    await screen.findByText("newer");
    expect(screen.getByText(formatDateDivider(day2))).toBeInTheDocument();
    expect(screen.getByText(formatDateDivider(day1))).toBeInTheDocument();
  });

  it("asks for confirmation, deletes, and removes the row on confirmed success", async () => {
    vi.mocked(fetchInstantSessions).mockResolvedValue([
      { id: "i1", title: "hi", createdAt: 1700000000, updatedAt: 1700000000 },
    ]);
    vi.mocked(deleteSession).mockResolvedValue(true);
    const confirmSpy = vi.spyOn(window, "confirm").mockReturnValue(true);
    const user = userEvent.setup();
    render(<InstantSessions />);

    await user.click(await screen.findByRole("button", { name: "인스턴트 대화 삭제" }));

    expect(confirmSpy).toHaveBeenCalled();
    expect(deleteSession).toHaveBeenCalledWith("i1");
    expect(await screen.findByText("아직 인스턴트 대화가 없습니다.")).toBeInTheDocument();
  });

  it("does not delete when the confirmation is declined", async () => {
    vi.mocked(fetchInstantSessions).mockResolvedValue([
      { id: "i1", title: "hi", createdAt: 1700000000, updatedAt: 1700000000 },
    ]);
    vi.spyOn(window, "confirm").mockReturnValue(false);
    const user = userEvent.setup();
    render(<InstantSessions />);

    await user.click(await screen.findByRole("button", { name: "인스턴트 대화 삭제" }));

    expect(deleteSession).not.toHaveBeenCalled();
    expect(screen.getByRole("button", { name: "인스턴트 대화 삭제" })).toBeInTheDocument();
  });

  it("keeps the row when the delete request fails", async () => {
    vi.mocked(fetchInstantSessions).mockResolvedValue([
      { id: "i1", title: "hi", createdAt: 1700000000, updatedAt: 1700000000 },
    ]);
    vi.mocked(deleteSession).mockResolvedValue(false);
    vi.spyOn(window, "confirm").mockReturnValue(true);
    const user = userEvent.setup();
    render(<InstantSessions />);

    await user.click(await screen.findByRole("button", { name: "인스턴트 대화 삭제" }));

    expect(deleteSession).toHaveBeenCalledWith("i1");
    expect(screen.getByRole("button", { name: "인스턴트 대화 삭제" })).toBeInTheDocument();
  });
});
