import { afterEach, describe, expect, it, vi } from "vitest";
import { answerArticle, deleteArticleInstance, drawArticle, fetchArticleInstances } from "./articles";

afterEach(() => {
  vi.unstubAllGlobals();
});

describe("drawArticle", () => {
  it("returns status ok with the draw on a successful response", async () => {
    const draw = { id: "i1", source: "BBC", title: "t", summary: "s", choices: ["a", "b", "c", "d"] };
    vi.stubGlobal("fetch", vi.fn().mockResolvedValue({ ok: true, status: 200, json: () => Promise.resolve(draw) }));
    await expect(drawArticle()).resolves.toEqual({ status: "ok", draw });
  });

  it("returns status noMore on a 204 (every article already drawn)", async () => {
    vi.stubGlobal("fetch", vi.fn().mockResolvedValue({ ok: true, status: 204 }));
    await expect(drawArticle()).resolves.toEqual({ status: "noMore" });
  });

  it("returns status error on a non-ok, non-204 response", async () => {
    vi.stubGlobal("fetch", vi.fn().mockResolvedValue({ ok: false, status: 500 }));
    await expect(drawArticle()).resolves.toEqual({ status: "error" });
  });

  it("returns status error when fetch rejects", async () => {
    vi.stubGlobal("fetch", vi.fn().mockRejectedValue(new Error("network down")));
    await expect(drawArticle()).resolves.toEqual({ status: "error" });
  });

  it("posts to api/articles/draw", async () => {
    const fetchMock = vi.fn().mockResolvedValue({ ok: true, status: 204 });
    vi.stubGlobal("fetch", fetchMock);
    await drawArticle();
    expect(fetchMock).toHaveBeenCalledWith("api/articles/draw", { method: "POST" });
  });
});

describe("fetchArticleInstances", () => {
  it("returns the list on a successful response", async () => {
    const list = [{ id: "i1", source: "BBC", title: "t", summary: "s", answered: false, correct: false, createdAt: 1 }];
    vi.stubGlobal("fetch", vi.fn().mockResolvedValue({ ok: true, json: () => Promise.resolve(list) }));
    await expect(fetchArticleInstances()).resolves.toEqual(list);
  });

  it("returns [] on a non-ok response", async () => {
    vi.stubGlobal("fetch", vi.fn().mockResolvedValue({ ok: false }));
    await expect(fetchArticleInstances()).resolves.toEqual([]);
  });

  it("returns [] when fetch rejects", async () => {
    vi.stubGlobal("fetch", vi.fn().mockRejectedValue(new Error("network down")));
    await expect(fetchArticleInstances()).resolves.toEqual([]);
  });
});

describe("answerArticle", () => {
  it("posts the selected index and returns the reveal", async () => {
    const result = { correct: true, correctIndex: 1, translation: "정확한 해석", explanation: "왜냐하면" };
    const fetchMock = vi.fn().mockResolvedValue({ ok: true, json: () => Promise.resolve(result) });
    vi.stubGlobal("fetch", fetchMock);

    await expect(answerArticle("i1", 1)).resolves.toEqual(result);
    expect(fetchMock).toHaveBeenCalledWith("api/articles/i1/answer", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ selectedIndex: 1 }),
    });
  });

  it("returns null on a non-ok response", async () => {
    vi.stubGlobal("fetch", vi.fn().mockResolvedValue({ ok: false }));
    await expect(answerArticle("i1", 0)).resolves.toBeNull();
  });

  it("returns null when fetch rejects", async () => {
    vi.stubGlobal("fetch", vi.fn().mockRejectedValue(new Error("network down")));
    await expect(answerArticle("i1", 0)).resolves.toBeNull();
  });

  it("URL-encodes the id", async () => {
    const fetchMock = vi.fn().mockResolvedValue({ ok: false });
    vi.stubGlobal("fetch", fetchMock);
    await answerArticle("weird id/1", 0);
    expect(fetchMock).toHaveBeenCalledWith(
      "api/articles/weird%20id%2F1/answer",
      expect.objectContaining({ method: "POST" }),
    );
  });
});

describe("deleteArticleInstance", () => {
  it("returns true on a successful response", async () => {
    const fetchMock = vi.fn().mockResolvedValue({ ok: true });
    vi.stubGlobal("fetch", fetchMock);
    await expect(deleteArticleInstance("i1")).resolves.toBe(true);
    expect(fetchMock).toHaveBeenCalledWith("api/articles/i1", { method: "DELETE" });
  });

  it("returns false on a non-ok response", async () => {
    vi.stubGlobal("fetch", vi.fn().mockResolvedValue({ ok: false }));
    await expect(deleteArticleInstance("i1")).resolves.toBe(false);
  });

  it("returns false when fetch rejects", async () => {
    vi.stubGlobal("fetch", vi.fn().mockRejectedValue(new Error("network down")));
    await expect(deleteArticleInstance("i1")).resolves.toBe(false);
  });
});
