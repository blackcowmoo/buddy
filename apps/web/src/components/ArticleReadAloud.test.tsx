/** @vitest-environment jsdom */
import "@testing-library/jest-dom/vitest";
import { act, cleanup, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { ArticleReadAloud } from "./ArticleReadAloud";

beforeEach(() => {
  vi.useFakeTimers();
  vi.spyOn(HTMLMediaElement.prototype, "play").mockResolvedValue();
  vi.spyOn(HTMLMediaElement.prototype, "pause").mockImplementation(() => {});
  vi.spyOn(console, "error").mockImplementation(() => {});
});

afterEach(() => {
  cleanup();
  vi.useRealTimers();
  vi.restoreAllMocks();
});

function pendingPlay() {
  let reject!: (error: Error) => void;
  const promise = new Promise<void>((_, rejectPlay) => { reject = rejectPlay; });
  return { promise, reject };
}

it("ignores a cancelled play request's late rejection after another playback starts", async () => {
  const firstPlay = pendingPlay();
  vi.mocked(HTMLMediaElement.prototype.play).mockReturnValueOnce(firstPlay.promise);
  const { container } = render(<ArticleReadAloud articleId="article" />);
  const audio = container.querySelector("audio")!;

  fireEvent.click(screen.getByRole("button", { name: "🔊 읽어주기" }));
  audio.currentTime = 12;
  fireEvent.click(screen.getByRole("button", { name: "취소" }));
  expect(audio.currentTime).toBe(0);
  expect(audio.pause).toHaveBeenCalledOnce();

  fireEvent.click(screen.getByRole("button", { name: "🔊 읽어주기" }));
  fireEvent.playing(audio);
  await act(async () => firstPlay.reject(new Error("cancelled")));
  expect(screen.getByRole("button", { name: "재생 중…" })).toBeDisabled();
  expect(console.error).not.toHaveBeenCalled();
});

it("recovers once when both the media event and play promise report the same error", async () => {
  const firstPlay = pendingPlay();
  const failure = new Error("unavailable");
  vi.mocked(HTMLMediaElement.prototype.play).mockReturnValueOnce(firstPlay.promise);
  const { container } = render(<ArticleReadAloud articleId="article" />);
  const audio = container.querySelector("audio")!;

  fireEvent.click(screen.getByRole("button", { name: "🔊 읽어주기" }));
  fireEvent.error(audio);
  act(() => vi.advanceTimersByTime(500));
  await act(async () => firstPlay.reject(failure));
  expect(console.error).toHaveBeenCalledWith("tts:", failure);
  expect(screen.getByRole("button", { name: "재생 실패, 다시 시도해주세요" })).toBeDisabled();

  act(() => vi.advanceTimersByTime(1500));
  fireEvent.click(screen.getByRole("button", { name: "🔊 읽어주기" }));
  fireEvent.playing(audio);
  act(() => vi.advanceTimersByTime(500));
  expect(screen.getByRole("button", { name: "재생 중…" })).toBeDisabled();
});

it("clears error recovery and stops the audio when the reading view unmounts", () => {
  const { container, unmount } = render(<ArticleReadAloud articleId="article" />);
  const audio = container.querySelector("audio")!;
  fireEvent.click(screen.getByRole("button", { name: "🔊 읽어주기" }));
  fireEvent.error(audio);
  expect(vi.getTimerCount()).toBe(1);

  unmount();
  expect(audio.pause).toHaveBeenCalledOnce();
  expect(vi.getTimerCount()).toBe(0);
});

it("ignores a play rejection after leaving the reading view", async () => {
  const pending = pendingPlay();
  vi.mocked(HTMLMediaElement.prototype.play).mockReturnValueOnce(pending.promise);
  const { unmount } = render(<ArticleReadAloud articleId="article" />);
  fireEvent.click(screen.getByRole("button", { name: "🔊 읽어주기" }));
  unmount();

  await act(async () => pending.reject(new Error("view closed")));
  expect(console.error).not.toHaveBeenCalled();
  expect(vi.getTimerCount()).toBe(0);
});
