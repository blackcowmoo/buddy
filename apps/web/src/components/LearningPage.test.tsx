/** @vitest-environment jsdom */
import "@testing-library/jest-dom/vitest";
import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, expect, it, vi } from "vitest";
import { LearningPage } from "./LearningPage";

afterEach(cleanup);

it("shares navigation and one scroll surface without resetting a refreshed view", () => {
  const { rerender } = render(<LearningPage title="학습" viewKey="list">기록</LearningPage>);
  const page = screen.getByRole("main");
  expect(page).toHaveClass("learning-page");
  expect(screen.getByRole("heading", { name: "학습" })).toBeInTheDocument();
  expect(screen.getByRole("button", { name: "메뉴" })).toBeInTheDocument();

  page.scrollTop = 240;
  rerender(<LearningPage title="학습" viewKey="list">갱신된 기록</LearningPage>);
  expect(screen.getByRole("main")).toBe(page);
  expect(page.scrollTop).toBe(240);

  rerender(<LearningPage title="학습" viewKey="detail">문제</LearningPage>);
  expect(page.scrollTop).toBe(0);
  page.scrollTop = 120;
  rerender(<LearningPage title="학습" viewKey="list">기록</LearningPage>);
  expect(page.scrollTop).toBe(0);
});

it("forwards scroll events from the page so history pagination still works", () => {
  const onScroll = vi.fn();
  render(<LearningPage title="학습" onScroll={onScroll}>기록</LearningPage>);
  const page = screen.getByRole("main");
  fireEvent.scroll(page);
  expect(onScroll).toHaveBeenCalledOnce();
  expect(onScroll.mock.calls[0][0].target).toBe(page);
});
