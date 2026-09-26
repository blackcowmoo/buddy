/** @vitest-environment jsdom */
import { render, screen } from "@testing-library/react";
import { expect, it } from "vitest";
import { newestFirst, useViewScrollTop } from "./listView";

it("orders a copied list from newest to oldest without mutating its source", () => {
  const source = [
    { id: "old", createdAt: 10 },
    { id: "new", createdAt: 30 },
    { id: "middle", createdAt: 20 },
  ];

  expect(newestFirst(source, (item) => item.createdAt).map((item) => item.id)).toEqual(["new", "middle", "old"]);
  expect(source.map((item) => item.id)).toEqual(["old", "new", "middle"]);
});

it("resets scroll for a view change but preserves it for an in-place rerender", () => {
  function Page({ view }: { view: string }) {
    const ref = useViewScrollTop<HTMLElement>(view);
    return <main ref={ref}>page</main>;
  }
  const { rerender } = render(<Page view="list" />);
  const page = screen.getByRole("main");

  page.scrollTop = 240;
  rerender(<Page view="list" />);
  expect(page.scrollTop).toBe(240);

  rerender(<Page view="detail" />);
  expect(page.scrollTop).toBe(0);
});
