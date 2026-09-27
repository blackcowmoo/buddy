/** @vitest-environment jsdom */
import "@testing-library/jest-dom/vitest";
import { cleanup, render, screen } from "@testing-library/react";
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { DatedList } from "./DatedList";

beforeEach(() => {
  vi.useFakeTimers();
  vi.setSystemTime(new Date(2024, 0, 2, 12));
});
afterEach(() => { cleanup(); vi.useRealTimers(); });

const timestamp = (day: number, hour: number, minute = 0) => new Date(2024, 0, day, hour, minute).getTime() / 1000;
const recent = { id: "recent", createdAt: timestamp(2, 0, 1) };
const older = { id: "older", createdAt: timestamp(1, 23, 59) };
const oldest = { id: "oldest", createdAt: timestamp(1, 12) };
const row = (item: { id: string }) => <button>{item.id}</button>;

it("inserts one divider per local calendar day, even across a two-minute midnight boundary", () => {
  render(<DatedList items={[recent, older, oldest]}>{row}</DatedList>);
  expect(screen.getAllByText("오늘")).toHaveLength(1);
  expect(screen.getAllByText("어제")).toHaveLength(1);
  expect(screen.getAllByRole("button").map((button) => button.textContent)).toEqual(["recent", "older", "oldest"]);
  expect(screen.getByText("어제").compareDocumentPosition(screen.getByText("older")) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
});

it("preserves row identity and avoids duplicate dates when pagination appends the same day", () => {
  const { rerender } = render(<DatedList items={[recent, older]}>{row}</DatedList>);
  const first = screen.getByRole("button", { name: "recent" });
  rerender(<DatedList items={[recent, older, oldest]}>{row}</DatedList>);
  expect(screen.getByRole("button", { name: "recent" })).toBe(first);
  expect(screen.getAllByText("어제")).toHaveLength(1);

  rerender(<DatedList items={[oldest]}>{row}</DatedList>);
  expect(screen.queryByText("오늘")).not.toBeInTheDocument();
  expect(screen.getAllByText("어제")).toHaveLength(1);
});

it("keeps the caller's order and renders no dividers for an empty list", () => {
  const { rerender, container } = render(<DatedList items={[older, recent]}>{row}</DatedList>);
  expect(screen.getAllByRole("button").map((button) => button.textContent)).toEqual(["older", "recent"]);
  rerender(<DatedList items={[]}>{row}</DatedList>);
  expect(container).toBeEmptyDOMElement();
});
