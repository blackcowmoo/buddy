import { afterEach, expect, it, vi } from "vitest";
import { checkWriting } from "./writing";
afterEach(() => vi.unstubAllGlobals());
it("includes the prompt identity so deletion cancels checking", async () => {
 const fetchMock = vi.fn().mockResolvedValue({ ok: true, json: async () => null });
 vi.stubGlobal("fetch", fetchMock);
 await checkWriting("문장", "sentence", "prompt-id");
 expect(JSON.parse(fetchMock.mock.calls[0][1].body)).toEqual({ prompt: "문장", answer: "sentence", promptId: "prompt-id" });
});
