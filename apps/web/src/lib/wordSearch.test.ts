import { beforeEach, describe, expect, it, vi } from "vitest";
import { checkDefinedWord, defineWord } from "./wordSearch";

describe("defineWord", () => {
  beforeEach(() => {
    vi.restoreAllMocks();
  });

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
});
