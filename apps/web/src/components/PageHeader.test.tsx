/** @vitest-environment jsdom */
import "@testing-library/jest-dom/vitest";
import { createRef } from "react";
import { cleanup, render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";
import { PageHeader } from "./PageHeader";

afterEach(cleanup);

describe("PageHeader", () => {
  it("keeps the title and page actions available while the menu is closed", async () => {
    const user = userEvent.setup();
    const onToggleMenu = vi.fn();
    const menuRef = createRef<HTMLDivElement>();
    render(
      <PageHeader brand={<h1>학습</h1>} actions={<button>추가 작업</button>} menuOpen={false} onToggleMenu={onToggleMenu} menuRef={menuRef}>
        <a href="words" role="menuitem">단어 복습</a>
      </PageHeader>,
    );

    expect(screen.getByRole("banner")).toContainElement(screen.getByRole("heading", { name: "학습" }));
    expect(screen.getByRole("button", { name: "추가 작업" })).toBeInTheDocument();
    const trigger = screen.getByRole("button", { name: "메뉴" });
    expect(trigger).toHaveAttribute("type", "button");
    expect(trigger).toHaveAttribute("aria-expanded", "false");
    expect(trigger).not.toHaveAttribute("aria-controls");
    expect(menuRef.current).toContainElement(trigger);
    expect(screen.queryByRole("menu")).not.toBeInTheDocument();

    await user.click(trigger);
    expect(onToggleMenu).toHaveBeenCalledOnce();
  });

  it("associates the expanded trigger with the visible menu", () => {
    render(
      <PageHeader brand={<h1>학습</h1>} menuOpen onToggleMenu={vi.fn()} menuRef={createRef<HTMLDivElement>()}>
        <a href="words" role="menuitem">단어 복습</a>
      </PageHeader>,
    );

    const trigger = screen.getByRole("button", { name: "메뉴" });
    const menu = screen.getByRole("menu");
    expect(trigger).toHaveAttribute("aria-expanded", "true");
    expect(trigger).toHaveAttribute("aria-controls", menu.id);
    expect(menu).toContainElement(screen.getByRole("menuitem", { name: "단어 복습" }));
  });
});
