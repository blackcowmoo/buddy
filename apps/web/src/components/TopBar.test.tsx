/** @vitest-environment jsdom */
import "@testing-library/jest-dom/vitest";
import { cleanup, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it } from "vitest";
import { TopBar } from "./TopBar";
import type { Theme } from "../lib/theme";

const noop = () => {};

function renderMenu(overrides: Partial<React.ComponentProps<typeof TopBar>> = {}) {
  const props: React.ComponentProps<typeof TopBar> = {
    brand: <h1>Buddy</h1>,
    menuOpen: true,
    onToggleMenu: noop,
    menuRef: { current: null },
    email: null,
    theme: "system" as Theme,
    onThemeChange: noop,
    prInput: "",
    onPrInputChange: noop,
    prError: false,
    currentPr: null,
    onGoToPath: noop,
    wordDueCount: 0,
    nuanceDueCount: 0,
    styleInput: "",
    onStyleInputChange: noop,
    styleSaving: false,
    styleSaved: false,
    styleLoadError: false,
    styleSaveError: null,
    onSubmitStyle: noop,
    learnerProfile: "",
    ...overrides,
  };
  render(<TopBar {...props} />);
}

afterEach(cleanup);

describe("TopBar PR-preview field", () => {
  it("shows the generic example placeholder on the root deployment", () => {
    renderMenu();
    expect(screen.getByLabelText("PR 미리보기로 이동")).toHaveAttribute(
      "placeholder",
      "PR 번호 (예: 14)",
    );
  });

  it("shows the current PR number in the placeholder during a PR preview", () => {
    renderMenu({ currentPr: "14" });
    expect(screen.getByLabelText("PR 미리보기로 이동")).toHaveAttribute("placeholder", "현재 PR: 14");
  });
});
