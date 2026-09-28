/** @vitest-environment jsdom */
import { act, cleanup, renderHook } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { usePollScaffold } from "./usePollScaffold";

beforeEach(() => vi.useFakeTimers());
afterEach(() => {
  cleanup();
  vi.useRealTimers();
});

async function advance(ms = 1000) {
  await act(async () => { await vi.advanceTimersByTimeAsync(ms); });
}

function setup() {
  const hook = renderHook(() => usePollScaffold());
  const token = {};
  hook.result.current.tokenRef.current = token;
  return { ...hook, token };
}

function deferred<T>() {
  let resolve!: (value: T) => void;
  const promise = new Promise<T>((done) => { resolve = done; });
  return { promise, resolve };
}

describe("usePollScaffold", () => {
  it("waits between settled requests without overlapping slow fetches", async () => {
    const { result, token } = setup();
    const pending = deferred<string>();
    const fetchResult = vi.fn().mockReturnValueOnce(pending.promise).mockResolvedValue("done");
    const onResult = vi.fn((status: string) => status !== "done");
    result.current.startPoll(token, { intervalMs: 1000, fetchResult, onResult });

    await advance(999);
    expect(fetchResult).not.toHaveBeenCalled();
    await advance(1);
    expect(fetchResult).toHaveBeenCalledTimes(1);
    await advance(5000);
    expect(fetchResult).toHaveBeenCalledTimes(1);
    expect(onResult).not.toHaveBeenCalled();

    await act(async () => { pending.resolve("pending"); });
    expect(onResult).toHaveBeenCalledExactlyOnceWith("pending");
    await advance(999);
    expect(fetchResult).toHaveBeenCalledTimes(1);
    await advance(1);
    expect(onResult).toHaveBeenLastCalledWith("done");
    await advance(5000);
    expect(fetchResult).toHaveBeenCalledTimes(2);
    expect(vi.getTimerCount()).toBe(0);
  });

  it.each([true, false])("lets the caller choose whether to retry a missing result: %s", async (retry) => {
    const { result, token } = setup();
    const fetchResult = vi.fn().mockResolvedValueOnce(null).mockResolvedValue("done");
    const onResult = vi.fn((value: string | null) => value === null && retry);
    result.current.startPoll(token, { intervalMs: 1000, fetchResult, onResult });

    await advance(5000);
    expect(onResult).toHaveBeenNthCalledWith(1, null);
    expect(fetchResult).toHaveBeenCalledTimes(retry ? 2 : 1);
    expect(vi.getTimerCount()).toBe(0);
  });

  it("applies the last allowed response before stopping at the attempt limit", async () => {
    const { result, token } = setup();
    const fetchResult = vi.fn().mockResolvedValue("pending");
    const onResult = vi.fn(() => true);
    result.current.startPoll(token, { intervalMs: 1000, maxAttempts: 3, fetchResult, onResult });

    await advance(10000);
    expect(fetchResult).toHaveBeenCalledTimes(3);
    expect(onResult).toHaveBeenCalledTimes(3);
    expect(vi.getTimerCount()).toBe(0);
  });

  it("does not fetch when the active token changes before the timer fires", async () => {
    const { result, token } = setup();
    const fetchResult = vi.fn().mockResolvedValue("done");
    const onResult = vi.fn(() => false);
    result.current.startPoll(token, { intervalMs: 1000, fetchResult, onResult });
    result.current.tokenRef.current = {};

    await advance();
    expect(fetchResult).not.toHaveBeenCalled();
    expect(onResult).not.toHaveBeenCalled();
    expect(vi.getTimerCount()).toBe(0);
  });

  it.each(["leave", "replace"])("ignores an in-flight response after %s", async (action) => {
    const { result, token } = setup();
    const pending = deferred<string>();
    const fetchResult = vi.fn(() => pending.promise);
    const onResult = vi.fn(() => true);
    result.current.startPoll(token, { intervalMs: 1000, fetchResult, onResult });
    await advance();

    result.current.tokenRef.current = action === "leave" ? null : {};
    await act(async () => { pending.resolve("pending"); });
    expect(onResult).not.toHaveBeenCalled();
    expect(vi.getTimerCount()).toBe(0);
  });

  it("keeps independent polls sharing a room token running until each finishes", async () => {
    const { result, token } = setup();
    const summary = vi.fn().mockResolvedValue("done");
    const quiz = vi.fn().mockResolvedValueOnce("pending").mockResolvedValue("done");
    const onResult = (status: string) => status !== "done";
    result.current.startPoll(token, { intervalMs: 1000, fetchResult: summary, onResult });
    result.current.startPoll(token, { intervalMs: 1000, fetchResult: quiz, onResult });

    await advance(5000);
    expect(summary).toHaveBeenCalledTimes(1);
    expect(quiz).toHaveBeenCalledTimes(2);
    expect(result.current.tokenRef.current).toBe(token);
  });

  it("clears scheduled polls and ignores requests still in flight on unmount", async () => {
    const { result, token, unmount } = setup();
    const pending = deferred<string>();
    const inFlight = vi.fn(() => pending.promise);
    const scheduled = vi.fn().mockResolvedValue("done");
    const onResult = vi.fn(() => true);
    result.current.startPoll(token, { intervalMs: 1000, fetchResult: inFlight, onResult });
    result.current.startPoll(token, { intervalMs: 2000, fetchResult: scheduled, onResult });
    await advance();
    expect(inFlight).toHaveBeenCalledTimes(1);

    unmount();
    expect(vi.getTimerCount()).toBe(0);
    await act(async () => { pending.resolve("pending"); });
    await advance(5000);
    expect(scheduled).not.toHaveBeenCalled();
    expect(onResult).not.toHaveBeenCalled();
    expect(vi.getTimerCount()).toBe(0);
  });
});
