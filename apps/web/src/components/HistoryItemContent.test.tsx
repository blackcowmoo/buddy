/** @vitest-environment jsdom */
import "@testing-library/jest-dom/vitest";
import { cleanup, render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, expect, it, vi } from "vitest";
import { HistoryItemContent } from "./HistoryItemContent";

afterEach(cleanup);

it("retains the full title and description while grouping badges and time separately", () => {
  const title = "pneumonoultramicroscopicsilicovolcanoconiosis".repeat(3);
  const description = "공백없는긴설명".repeat(30);
  render(<a href=".#chat/room">
    <HistoryItemContent title={title} description={description} badges={<span>새 피드백 123</span>} meta="오후 1:00" />
  </a>);
  const link = screen.getByRole("link");
  const titleElement = within(link).getByText(title);
  const meta = within(link).getByText("오후 1:00").closest(".history-item-meta")!;
  expect(titleElement).toHaveAttribute("title", title);
  expect(within(link).getByText(description)).toBeInTheDocument();
  expect(meta).toContainElement(within(link).getByText("새 피드백 123"));
  expect(meta).not.toContainElement(titleElement);
  expect(link).toHaveAttribute("href", ".#chat/room");
});

it("leaves opening and deletion as separate controls", async () => {
  const onOpen = vi.fn();
  const onDelete = vi.fn();
  const user = userEvent.setup();
  render(<div className="session-row">
    <button onClick={onOpen}><HistoryItemContent title="기록" meta="오늘" /></button>
    <button onClick={onDelete} aria-label="기록 삭제">🗑</button>
  </div>);
  await user.click(screen.getByRole("button", { name: "기록 오늘" }));
  expect(onOpen).toHaveBeenCalledOnce();
  await user.click(screen.getByRole("button", { name: "기록 삭제" }));
  expect(onDelete).toHaveBeenCalledOnce();
  expect(onOpen).toHaveBeenCalledOnce();
});
