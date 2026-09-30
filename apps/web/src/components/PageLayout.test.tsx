/** @vitest-environment jsdom */
import "@testing-library/jest-dom/vitest";
import { cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, expect, it, vi } from "vitest";
import { AppShell, BackButton, PageContent, PageSection, PageToolbar } from "./PageLayout";

afterEach(cleanup);

it("lets keyboard users skip the header without changing the room history hash", async () => {
  const user = userEvent.setup();
  const url = window.location.href;
  render(
    <AppShell header={<header><button>메뉴</button></header>} footer={<footer><input aria-label="메시지" /></footer>}>
      <PageContent><button>콘텐츠 동작</button></PageContent>
    </AppShell>,
  );
  expect(screen.getAllByRole("main")).toHaveLength(1);
  expect(within(screen.getByRole("main")).queryByRole("banner")).not.toBeInTheDocument();
  expect(within(screen.getByRole("main")).queryByRole("contentinfo")).not.toBeInTheDocument();
  await user.tab();
  expect(screen.getByRole("button", { name: "본문으로 건너뛰기" })).toHaveFocus();
  await user.keyboard("{Enter}");
  expect(screen.getByRole("main")).toHaveFocus();
  expect(window.location.href).toBe(url);
  await user.tab();
  expect(screen.getByRole("button", { name: "콘텐츠 동작" })).toHaveFocus();
});

it("preserves the scroll surface on refresh, resets on navigation, and forwards pagination events", () => {
  // React clears currentTarget after dispatch, so capture it in the handler.
  const onScroll = vi.fn((event) => event.currentTarget);
  const { rerender } = render(<PageContent viewKey="list" onScroll={onScroll}>기록</PageContent>);
  const page = screen.getByRole("main");
  page.scrollTop = 180;
  rerender(<PageContent viewKey="list" onScroll={onScroll}>갱신된 기록</PageContent>);
  expect(page.scrollTop).toBe(180);
  fireEvent.scroll(page);
  expect(onScroll).toHaveBeenCalledOnce();
  expect(onScroll.mock.results[0].value).toBe(page);
  rerender(<PageContent viewKey="detail" onScroll={onScroll}>상세</PageContent>);
  expect(screen.getByRole("main")).toBe(page);
  expect(page.scrollTop).toBe(0);
});

it("associates each section with its own heading and keeps section actions discoverable", async () => {
  const user = userEvent.setup();
  const retry = vi.fn();
  render(<>
    <PageSection title="학습 기록" description="최근 기록부터 보여요" actions={<button onClick={retry}>새로고침</button>}>
      첫 번째 기록
    </PageSection>
    <PageSection title="복습 기록">두 번째 기록</PageSection>
  </>);
  const history = screen.getByRole("region", { name: "학습 기록" });
  expect(within(history).getByRole("heading", { level: 2, name: "학습 기록" })).toBeInTheDocument();
  expect(within(history).getByText("최근 기록부터 보여요")).toBeInTheDocument();
  expect(within(history).getByText("첫 번째 기록")).toBeInTheDocument();
  expect(within(history).queryByText("두 번째 기록")).not.toBeInTheDocument();
  await user.click(within(history).getByRole("button", { name: "새로고침" }));
  expect(retry).toHaveBeenCalledOnce();
});

it("keeps primary actions in a named group and return actions usable without submitting a form", async () => {
  const user = userEvent.setup();
  const back = vi.fn();
  const submit = vi.fn((event) => event.preventDefault());
  const { rerender } = render(<form onSubmit={submit}><PageToolbar label="학습 작업">
    <BackButton onClick={back} disabled />
    <button type="button">복습 시작</button>
  </PageToolbar></form>);
  expect(within(screen.getByRole("group", { name: "학습 작업" })).getAllByRole("button")).toHaveLength(2);
  await user.click(screen.getByRole("button", { name: "목록으로" }));
  expect(back).not.toHaveBeenCalled();
  rerender(<form onSubmit={submit}><BackButton onClick={back} /></form>);
  await user.click(screen.getByRole("button", { name: "목록으로" }));
  expect(back).toHaveBeenCalledOnce();
  expect(submit).not.toHaveBeenCalled();
  rerender(<BackButton href="nuance">뉘앙스 목록으로</BackButton>);
  expect(screen.getByRole("link", { name: "뉘앙스 목록으로" })).toHaveAttribute("href", "nuance");
});
