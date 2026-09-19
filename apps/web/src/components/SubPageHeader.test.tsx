/**
 * @vitest-environment jsdom
 */
import "@testing-library/jest-dom/vitest";
import { cleanup, render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it } from "vitest";

import { SubPageHeader } from "./SubPageHeader";

afterEach(cleanup);

describe("SubPageHeader", () => {
  it("keeps the page title clear until the navigation menu is opened", () => {
    render(<SubPageHeader title="단어 복습" />);
    expect(screen.getByRole("heading", { name: "단어 복습" })).toBeInTheDocument();
    expect(screen.queryByRole("link", { name: "홈으로" })).not.toBeInTheDocument();
    expect(screen.queryByRole("menu")).not.toBeInTheDocument();
  });

  it("opens the complete navigation list and keeps the home link relative", async () => {
    const user = userEvent.setup();
    render(<SubPageHeader title="오늘의 아티클" />);

    const menuButton = screen.getByRole("button", { name: "메뉴" });
    expect(menuButton).toHaveAttribute("aria-expanded", "false");
    expect(screen.queryByRole("menu")).not.toBeInTheDocument();

    await user.click(menuButton);

    const menu = screen.getByRole("menu");
    expect(menuButton).toHaveAttribute("aria-expanded", "true");
    expect(menu).toBeInTheDocument();
    expect(screen.getAllByRole("menuitem")).toHaveLength(8);
    expect(screen.getByRole("menuitem", { name: "메인으로" })).toHaveAttribute("href", ".");
    for (const [name, href] of [
      ["녹음 목록", "recordings"],
      ["인스턴트 대화 목록", "instant"],
      ["단어 복습", "words"],
      ["단어 매칭 게임", "match"],
      ["단어 뉘앙스", "nuance"],
      ["오늘의 아티클", "article"],
      ["오늘의 작문", "writing"],
    ]) {
      expect(screen.getByRole("menuitem", { name: new RegExp(name) })).toHaveAttribute("href", href);
    }
  });

  it("closes the menu with Escape", async () => {
    const user = userEvent.setup();
    render(<SubPageHeader title="단어 뉘앙스" />);

    await user.click(screen.getByRole("button", { name: "메뉴" }));
    await user.keyboard("{Escape}");

    expect(screen.queryByRole("menu")).not.toBeInTheDocument();
    expect(screen.getByRole("button", { name: "메뉴" })).toHaveAttribute("aria-expanded", "false");
  });
});
