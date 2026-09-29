import { afterEach, describe, expect, it, vi } from "vitest";
import { confirmResearchWord, deleteWord, fetchAutoAddStatus, fetchWords, findSameWordEntries, reviewWord, saveWord, selectResearchWord, selectWordMeaning, startAutoAddWords, startResearchWord, wordStudyStatus, type WordReviewItem } from "./wordReview";

afterEach(() => {
  vi.unstubAllGlobals();
});

const suggestion = { word: "ecstatic", meaning: "매우 행복한", example: "She was ecstatic." };
const item = { id: "w1", ...suggestion, stage: 0, reviewCount: 0, nextReviewAt: 1700000000 };

it("selects a research result with the revision the learner actually saw", async () => {
  const word: WordReviewItem = { ...item, id: "w/a", status: "rejected", researchRevision: 3 };
  const request = vi.fn().mockResolvedValue({ ok: true, json: async () => word });
  vi.stubGlobal("fetch", request);
  await expect(selectResearchWord(word, suggestion)).resolves.toEqual(word);
  expect(request).toHaveBeenCalledWith("api/words/w%2Fa/research/select", expect.objectContaining({
    method: "POST", body: JSON.stringify({ revision: 3, suggestion }),
  }));
});

describe("word study status", () => {
  const confirmed: WordReviewItem = { ...item, status: "verified", researchStatus: "confirmed" };

  it.each([undefined, "confirmed"] as const)("keeps a confirmed word available with meaning status %s", (meaningStatus) => {
    expect(wordStudyStatus({ ...confirmed, meaningStatus })).toBe("confirmed");
  });

  it.each(["pending", "done", "failed"] as const)("reopens the decision during %s meaning cleanup", (meaningStatus) => {
    expect(wordStudyStatus({ ...confirmed, meaningStatus })).toBe("unconfirmed");
  });

  it.each([undefined, "pending", "done"] as const)("requires explicit confirmation after research status %s", (researchStatus) => {
    expect(wordStudyStatus({ ...confirmed, researchStatus, meaningStatus: "confirmed" })).toBe("unconfirmed");
  });

  it("waits for verification even if research and the meaning were confirmed", () => {
    expect(wordStudyStatus({ ...confirmed, status: "pending", meaningStatus: "confirmed" })).toBe("unconfirmed");
  });

  it("keeps rejected words excluded regardless of their confirmation or cleanup state", () => {
    expect(wordStudyStatus({ ...confirmed, status: "rejected", meaningStatus: "confirmed" })).toBe("rejected");
    expect(wordStudyStatus({ ...confirmed, status: "rejected", meaningStatus: "pending" })).toBe("rejected");
  });
});

describe("findSameWordEntries", () => {
  const learning: WordReviewItem = { ...item, status: "verified", researchStatus: "confirmed" };

  it("matches English phrases across casing and whitespace regardless of meaning", () => {
    const first = { ...learning, word: " Set  UP\t", meaning: "설치하다" };
    const second = { ...learning, id: "w2", word: "set up", meaning: "준비하다" };
    const joined = { ...learning, id: "w3", word: "setup" };
    const inflected = { ...learning, id: "w4", word: "sets up" };
    expect(findSameWordEntries([first, second, joined, inflected])).toEqual(new Map([
      [first.id, [second]],
      [second.id, [first]],
    ]));
  });

  it("includes pending entries and matching meanings without matching an item to itself", () => {
    const pending: WordReviewItem = { ...learning, id: "w2", status: "pending", researchStatus: undefined };
    const unconfirmed = { ...learning, id: "w3", meaning: "황홀한", researchStatus: undefined };
    expect(findSameWordEntries([learning])).toEqual(new Map());
    expect(findSameWordEntries([learning, pending, unconfirmed])).toEqual(new Map([
      [learning.id, [pending, unconfirmed]],
      [pending.id, [learning, unconfirmed]],
      [unconfirmed.id, [learning, pending]],
    ]));
  });

  it("shows existing study entries on rejected items without counting rejected items as matches", () => {
    const rejected: WordReviewItem = { ...learning, id: "w2", status: "rejected", meaning: "잘못된 뜻" };
    expect(findSameWordEntries([learning, rejected])).toEqual(new Map([[rejected.id, [learning]]]));
    expect(findSameWordEntries([rejected, { ...rejected, id: "w3" }])).toEqual(new Map());
  });

  it("does not group missing English text", () => {
    expect(findSameWordEntries([{ ...learning, word: "" }, { ...learning, id: "w2", word: " \t " }])).toEqual(new Map());
  });
});

it.each(["cleaned", "original"] as const)("posts the %s meaning choice with its persisted revision", async (choice) => {
  const word: WordReviewItem = { ...item, id: "w/a", status: "verified", meaningStatus: "done", meaningRevision: 4 };
  const request = vi.fn().mockResolvedValue({ ok: true, json: async () => word });
  vi.stubGlobal("fetch", request);
  await expect(selectWordMeaning(word, choice)).resolves.toEqual(word);
  expect(request).toHaveBeenCalledWith("api/words/w%2Fa/meaning", expect.objectContaining({ method: "POST", body: JSON.stringify({ choice, revision: 4 }) }));
});

it("reports a failed meaning selection without pretending it was saved", async () => {
  const word: WordReviewItem = { ...item, status: "verified", meaningStatus: "done" };
  const request = vi.fn().mockResolvedValueOnce({ ok: false }).mockRejectedValueOnce(new Error("network"));
  vi.stubGlobal("fetch", request);
  await expect(selectWordMeaning(word, "cleaned")).resolves.toBeNull();
  expect(request).toHaveBeenCalledWith("api/words/w1/meaning", expect.objectContaining({ body: JSON.stringify({ choice: "cleaned", revision: 0 }) }));
  await expect(selectWordMeaning(word, "original")).resolves.toBeNull();
});

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

describe("async research", () => {
  it("starts a persisted research job and returns its pending item", async () => {
    const pending = { ...item, researchStatus: "pending" };
    const fetchMock = vi.fn().mockResolvedValue({ ok: true, json: () => Promise.resolve(pending) });
    vi.stubGlobal("fetch", fetchMock);
    await expect(startResearchWord("w1")).resolves.toEqual(pending);
    expect(fetchMock).toHaveBeenCalledWith("api/words/w1/research", expect.objectContaining({ method: "POST" }));
  });

  it("confirms a research result so the control can stay hidden", async () => {
    const confirmed = { ...item, researchStatus: "confirmed" };
    vi.stubGlobal("fetch", vi.fn().mockResolvedValue({ ok: true, json: () => Promise.resolve(confirmed) }));
    await expect(confirmResearchWord("w1")).resolves.toEqual(confirmed);
  });
});

describe("reviewWord", () => {
  it("posts the answer and returns the updated item", async () => {
    const fetchMock = vi.fn().mockResolvedValue({ ok: true, json: () => Promise.resolve(item) });
    vi.stubGlobal("fetch", fetchMock);

    await expect(reviewWord("weird id/1", true, false, 1)).resolves.toEqual(item);
    expect(fetchMock).toHaveBeenCalledWith("api/words/weird%20id%2F1/review", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ correct: true, repeat: false, questionVersion: 1 }),
    });
  });

  // repeat marks a correct-but-forced-guess answer (the "억지로 맞췄어요"
  // button) so the word gets rescheduled at the same interval instead of
  // advancing — see httpserver.wordReviewHandler.
  it("posts the repeat flag when set", async () => {
    const fetchMock = vi.fn().mockResolvedValue({ ok: true, json: () => Promise.resolve(item) });
    vi.stubGlobal("fetch", fetchMock);

    await expect(reviewWord("w1", true, true, 1)).resolves.toEqual(item);
    expect(fetchMock).toHaveBeenCalledWith("api/words/w1/review", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ correct: true, repeat: true, questionVersion: 1 }),
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

describe("startAutoAddWords", () => {
  it("posts with no body and returns the job status", async () => {
    const fetchMock = vi.fn().mockResolvedValue({ ok: true, json: () => Promise.resolve({ status: "pending", count: 0 }) });
    vi.stubGlobal("fetch", fetchMock);

    await expect(startAutoAddWords()).resolves.toEqual({ status: "pending", count: 0 });
    expect(fetchMock).toHaveBeenCalledWith("api/words/auto-add", { method: "POST" });
  });

  it("returns null on a non-ok response", async () => {
    vi.stubGlobal("fetch", vi.fn().mockResolvedValue({ ok: false }));
    await expect(startAutoAddWords()).resolves.toBeNull();
  });

  it("returns null when fetch rejects", async () => {
    vi.stubGlobal("fetch", vi.fn().mockRejectedValue(new Error("network down")));
    await expect(startAutoAddWords()).resolves.toBeNull();
  });
});

describe("fetchAutoAddStatus", () => {
  it("returns the current job status on a successful response", async () => {
    const body = { status: "done", count: 2 };
    vi.stubGlobal("fetch", vi.fn().mockResolvedValue({ ok: true, json: () => Promise.resolve(body) }));
    await expect(fetchAutoAddStatus()).resolves.toEqual(body);
  });

  it("returns null on a non-ok response", async () => {
    vi.stubGlobal("fetch", vi.fn().mockResolvedValue({ ok: false }));
    await expect(fetchAutoAddStatus()).resolves.toBeNull();
  });

  it("returns null when fetch rejects", async () => {
    vi.stubGlobal("fetch", vi.fn().mockRejectedValue(new Error("network down")));
    await expect(fetchAutoAddStatus()).resolves.toBeNull();
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
