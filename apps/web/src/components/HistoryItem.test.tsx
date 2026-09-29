/** @vitest-environment jsdom */
import "@testing-library/jest-dom/vitest";
import { cleanup, render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, expect, it, vi } from "vitest";
import { HistoryItem } from "./HistoryItem";
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

it("opens by keyboard and keeps deletion as a separate control", async () => {
  const onOpen = vi.fn();
  const onDelete = vi.fn();
  const onSubmit = vi.fn((event: React.FormEvent) => event.preventDefault());
  const user = userEvent.setup();
  render(<form onSubmit={onSubmit}>
    <HistoryItem title="기록" meta="오늘" onOpen={onOpen} onDelete={onDelete} deleteLabel="기록 삭제" />
  </form>);
  await user.tab();
  expect(screen.getByRole("button", { name: "기록 오늘" })).toHaveFocus();
  await user.keyboard("{Enter}");
  expect(onOpen).toHaveBeenCalledOnce();
  await user.click(screen.getByRole("button", { name: "기록 삭제" }));
  expect(onDelete).toHaveBeenCalledOnce();
  expect(onOpen).toHaveBeenCalledOnce();
  expect(onSubmit).not.toHaveBeenCalled();
});

it("preserves native links and never puts the delete button inside one", async () => {
  const onDelete = vi.fn();
  const user = userEvent.setup();
  render(<HistoryItem title="대화" meta="오늘" href=".#chat/room%2F1" onDelete={onDelete} deleteLabel="대화 삭제" />);

  const link = screen.getByRole("link", { name: "대화 오늘" });
  const remove = screen.getByRole("button", { name: "대화 삭제" });
  expect(link).toHaveAttribute("href", ".#chat/room%2F1");
  expect(link).not.toContainElement(remove);
  await user.tab();
  expect(link).toHaveFocus();
  await user.tab();
  expect(remove).toHaveFocus();
  await user.keyboard("{Enter}");
  expect(onDelete).toHaveBeenCalledOnce();
});

it("blocks both actions while busy and retains the caller's accessible labels", async () => {
  const onOpen = vi.fn();
  const onDelete = vi.fn();
  const user = userEvent.setup();
  render(<HistoryItem title="비교" description="두 단어의 차이" meta="학습 중"
    onOpen={onOpen} openLabel="비교 열기" onDelete={onDelete} deleteLabel="비교 삭제" disabled />);

  const open = screen.getByRole("button", { name: "비교 열기" });
  const remove = screen.getByRole("button", { name: "비교 삭제" });
  expect(within(open).getByText("두 단어의 차이")).toBeInTheDocument();
  expect(open).toBeDisabled();
  expect(remove).toBeDisabled();
  await user.click(open);
  await user.click(remove);
  expect(onOpen).not.toHaveBeenCalled();
  expect(onDelete).not.toHaveBeenCalled();
});
