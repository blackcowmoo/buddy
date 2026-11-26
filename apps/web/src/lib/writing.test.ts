import { afterEach, expect, it, vi } from "vitest";
import { checkWriting, recheckWriting } from "./writing";

afterEach(() => vi.unstubAllGlobals());

// Submitting carries only the identity and the draft: the grade is produced
// by the detached server-side job, so the client must not send the prompt
// text or expect a correction back.
it("submits only the prompt identity and answer, returning the pending prompt", async () => {
  const pending = { id: "prompt-id", korean: "문장", status: "done", createdAt: 1, answer: "sentence", checkStatus: "pending" };
  const fetchMock = vi.fn().mockResolvedValue({ ok: true, json: async () => pending });
  vi.stubGlobal("fetch", fetchMock);
  await expect(checkWriting("prompt-id", "sentence")).resolves.toEqual(pending);
  expect(fetchMock.mock.calls[0][0]).toBe("api/writing/check");
  expect(JSON.parse(fetchMock.mock.calls[0][1].body)).toEqual({ promptId: "prompt-id", answer: "sentence" });
});

it("re-checks the stored answer through the prompt's recheck route", async () => {
  const pending = { id: "prompt id/odd", korean: "문장", status: "done", createdAt: 1, answer: "sentence", checkStatus: "pending" };
  const fetchMock = vi.fn().mockResolvedValue({ ok: true, json: async () => pending });
  vi.stubGlobal("fetch", fetchMock);
  await expect(recheckWriting("prompt id/odd")).resolves.toEqual(pending);
  expect(fetchMock.mock.calls[0][0]).toBe("api/writing/prompt%20id%2Fodd/recheck");
  expect(JSON.parse(fetchMock.mock.calls[0][1].body)).toEqual({});
});
