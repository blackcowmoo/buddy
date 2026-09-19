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
  it("offers a direct home link without opening the menu, including preview deployments", () => {
    render(<SubPageHeader title="단어 복습" />);
    const home = screen.getByRole("link", { name: "홈으로" });
    expect(home).toHaveAttribute("href", ".");
    expect(new URL(home.getAttribute("href")!, "https://buddy.test/pr/14/words").pathname).toBe("/pr/14/");
    expect(screen.queryByRole("menu")).not.toBeInTheDocument();
  });

  it("opens a hamburger menu containing only a relative link to the main page", async () => {
    const user = userEvent.setup();
    render(<SubPageHeader title="오늘의 아티클" />);

    const menuButton = screen.getByRole("button", { name: "메뉴" });
    expect(menuButton).toHaveAttribute("aria-expanded", "false");
    expect(screen.queryByRole("menu")).not.toBeInTheDocument();

    await user.click(menuButton);

    const menu = screen.getByRole("menu");
    expect(menuButton).toHaveAttribute("aria-expanded", "true");
    expect(menu).toBeInTheDocument();
    expect(screen.getAllByRole("menuitem")).toHaveLength(1);
    expect(screen.getByRole("menuitem", { name: "메인으로" })).toHaveAttribute("href", ".");
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
