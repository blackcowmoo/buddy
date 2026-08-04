import { afterEach, describe, expect, it, vi } from "vitest";
import { autoAddWords, deleteWord, fetchWords, reviewWord, saveWord } from "./wordReview";

afterEach(() => {
  vi.unstubAllGlobals();
});

const suggestion = { word: "ecstatic", meaning: "매우 행복한", example: "She was ecstatic." };
const item = { id: "w1", ...suggestion, stage: 0, reviewCount: 0, nextReviewAt: 1700000000 };

describe("saveWord", () => {
  it("posts the suggestion and returns the saved item", async () => {
    const fetchMock = vi.fn().mockResolvedValue({ ok: true, json: () => Promise.resolve(item) });
    vi.stubGlobal("fetch", fetchMock);

    await expect(saveWord(suggestion)).resolves.toEqual(item);
    expect(fetchMock).toHaveBeenCalledWith("api/words/save", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(suggestion),
    });
  });

  it("returns null on a non-ok response", async () => {
    vi.stubGlobal("fetch", vi.fn().mockResolvedValue({ ok: false }));
    await expect(saveWord(suggestion)).resolves.toBeNull();
  });

  it("returns null when fetch rejects", async () => {
    vi.stubGlobal("fetch", vi.fn().mockRejectedValue(new Error("network down")));
    await expect(saveWord(suggestion)).resolves.toBeNull();
  });
});

describe("fetchWords", () => {
  it("returns the words list and due count on a successful response", async () => {
    const body = { words: [item], dueCount: 1 };
    vi.stubGlobal("fetch", vi.fn().mockResolvedValue({ ok: true, json: () => Promise.resolve(body) }));
    await expect(fetchWords()).resolves.toEqual(body);
  });

  it("returns null on a non-ok response", async () => {
    vi.stubGlobal("fetch", vi.fn().mockResolvedValue({ ok: false }));
    await expect(fetchWords()).resolves.toBeNull();
  });

  it("returns null when fetch rejects", async () => {
    vi.stubGlobal("fetch", vi.fn().mockRejectedValue(new Error("network down")));
    await expect(fetchWords()).resolves.toBeNull();
  });
});

describe("reviewWord", () => {
  it("posts the answer and returns the updated item", async () => {
    const fetchMock = vi.fn().mockResolvedValue({ ok: true, json: () => Promise.resolve(item) });
    vi.stubGlobal("fetch", fetchMock);

    await expect(reviewWord("weird id/1", true)).resolves.toEqual(item);
    expect(fetchMock).toHaveBeenCalledWith("api/words/weird%20id%2F1/review", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ correct: true }),
    });
  });

  it("returns null on a non-ok response", async () => {
    vi.stubGlobal("fetch", vi.fn().mockResolvedValue({ ok: false }));
    await expect(reviewWord("w1", false)).resolves.toBeNull();
  });

  it("returns null when fetch rejects", async () => {
    vi.stubGlobal("fetch", vi.fn().mockRejectedValue(new Error("network down")));
    await expect(reviewWord("w1", false)).resolves.toBeNull();
  });
});

describe("autoAddWords", () => {
  it("posts with no body and returns the saved words", async () => {
    const fetchMock = vi.fn().mockResolvedValue({ ok: true, json: () => Promise.resolve({ words: [item] }) });
    vi.stubGlobal("fetch", fetchMock);

    await expect(autoAddWords()).resolves.toEqual([item]);
    expect(fetchMock).toHaveBeenCalledWith("api/words/auto-add", { method: "POST" });
  });

  it("returns an empty array when the server found nothing to add", async () => {
    vi.stubGlobal("fetch", vi.fn().mockResolvedValue({ ok: true, json: () => Promise.resolve({ words: [] }) }));
    await expect(autoAddWords()).resolves.toEqual([]);
  });

  it("returns null on a non-ok response", async () => {
    vi.stubGlobal("fetch", vi.fn().mockResolvedValue({ ok: false }));
    await expect(autoAddWords()).resolves.toBeNull();
  });

  it("returns null when fetch rejects", async () => {
    vi.stubGlobal("fetch", vi.fn().mockRejectedValue(new Error("network down")));
    await expect(autoAddWords()).resolves.toBeNull();
  });
});

describe("deleteWord", () => {
  it("sends a DELETE request and returns true on success", async () => {
    const fetchMock = vi.fn().mockResolvedValue({ ok: true });
    vi.stubGlobal("fetch", fetchMock);

    await expect(deleteWord("weird id/1")).resolves.toBe(true);
    expect(fetchMock).toHaveBeenCalledWith("api/words/weird%20id%2F1", { method: "DELETE" });
  });

  it("returns false on a non-ok response", async () => {
    vi.stubGlobal("fetch", vi.fn().mockResolvedValue({ ok: false }));
    await expect(deleteWord("w1")).resolves.toBe(false);
  });

  it("returns false when fetch rejects", async () => {
    vi.stubGlobal("fetch", vi.fn().mockRejectedValue(new Error("network down")));
    await expect(deleteWord("w1")).resolves.toBe(false);
  });
});
