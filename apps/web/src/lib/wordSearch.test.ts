import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { checkDefinedWord, checkDefinedWordStatus, defineWord } from "./wordSearch";

beforeEach(() => {
  vi.restoreAllMocks();
});

afterEach(() => {
  vi.useRealTimers();
});

describe("defineWord", () => {
  it("waits for the background result after starting the durable lookup", async () => {
    const fetchMock = vi
      .spyOn(globalThis, "fetch")
      .mockResolvedValueOnce(new Response(JSON.stringify({ status: "pending" }), { status: 200 }))
      .mockResolvedValueOnce(
        new Response(JSON.stringify({ status: "done", result: { word: "run", meaning: "운영하다", example: "They run the company." } }), {
          status: 200,
        }),
      );

    const resultPromise = defineWord("article/1", "run", 2);
    await vi.waitFor(() => expect(fetchMock).toHaveBeenCalledTimes(2), { timeout: 2500 });

    await expect(resultPromise).resolves.toEqual({ word: "run", meaning: "운영하다", example: "They run the company." });
    expect(fetchMock.mock.calls[0][0]).toBe("api/articles/article%2F1/words/define");
    expect(fetchMock.mock.calls[0][1]).toMatchObject({
      method: "POST",
      body: JSON.stringify({ word: "run", position: 2 }),
    });
  });

  it("keeps polling after a transient network failure", async () => {
    vi.useFakeTimers();
    const fetchMock = vi
      .spyOn(globalThis, "fetch")
      .mockRejectedValueOnce(new TypeError("network changed during deployment"))
      .mockResolvedValueOnce(
        new Response(JSON.stringify({ status: "done", result: { word: "run", meaning: "운영하다", example: "They run it." } }), {
          status: 200,
        }),
      );

    const resultPromise = defineWord("a1", "run", 2);
    await vi.waitFor(() => expect(fetchMock).toHaveBeenCalledTimes(1));
    await vi.advanceTimersByTimeAsync(1000);

    await expect(resultPromise).resolves.toEqual({ word: "run", meaning: "운영하다", example: "They run it." });
    expect(fetchMock).toHaveBeenCalledTimes(2);
  });
});

describe("checkDefinedWord", () => {
  it("asks the server for a cached result without starting a lookup", async () => {
    const fetchMock = vi.spyOn(globalThis, "fetch").mockResolvedValue(
      new Response(JSON.stringify({ status: "done", result: { word: "run", meaning: "운영하다", example: "They run the company." } }), {
        status: 200,
      }),
    );

    await expect(checkDefinedWord("article/1", "run", 2)).resolves.toEqual({
      word: "run",
      meaning: "운영하다",
      example: "They run the company.",
    });
    expect(fetchMock.mock.calls[0][1]).toMatchObject({
      method: "POST",
    });
    expect(fetchMock.mock.calls[0][0]).toBe("api/articles/article%2F1/words/define");
  });

  it("exposes an in-flight durable lookup separately from a cache miss", async () => {
    vi.spyOn(globalThis, "fetch").mockResolvedValue(
      new Response(JSON.stringify({ status: "pending" }), { status: 200 }),
    );

    await expect(checkDefinedWordStatus("article/1", "run", 2)).resolves.toEqual({ status: "pending" });
  });
});
