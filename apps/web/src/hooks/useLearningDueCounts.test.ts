/** @vitest-environment jsdom */
import { cleanup, renderHook, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { nuanceFixture } from "../lib/nuance.testData";

vi.mock("../lib/wordReview", () => ({ fetchWords: vi.fn() }));
vi.mock("../lib/nuance", async (importOriginal) => ({
  ...(await importOriginal<typeof import("../lib/nuance")>()),
  fetchNuanceLessons: vi.fn(),
}));

import { fetchNuanceLessons } from "../lib/nuance";
import { fetchWords } from "../lib/wordReview";
import { useLearningDueCounts } from "./useLearningDueCounts";

afterEach(cleanup);

beforeEach(() => {
  vi.mocked(fetchWords).mockResolvedValue({ words: [], dueCount: 5 });
  const lesson = nuanceFixture();
  lesson.state.progress.q0 = { stage: 1, attempts: 1, correct: 1, lastReviewedAt: 100, nextReviewAt: Number.MAX_SAFE_INTEGER };
  vi.mocked(fetchNuanceLessons).mockResolvedValue([lesson]);
});

describe("useLearningDueCounts", () => {
  it("loads the independently due word and nuance question totals", async () => {
    const { result } = renderHook(() => useLearningDueCounts());

    await waitFor(() => expect(result.current).toEqual({ wordDueCount: 5, nuanceDueCount: 3 }));
  });
});
