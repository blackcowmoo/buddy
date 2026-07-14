import { afterEach, describe, expect, it, vi } from "vitest";
import { deleteRecording, fetchRecordings, recordingAudioURL } from "./recordings";

afterEach(() => {
  vi.unstubAllGlobals();
});

describe("fetchRecordings", () => {
  it("returns the list on a successful response", async () => {
    const list = [{ id: "rec-1", createdAt: 1700000000, durationMs: 1500, sizeBytes: 4096 }];
    vi.stubGlobal(
      "fetch",
      vi.fn().mockResolvedValue({ ok: true, json: () => Promise.resolve(list) }),
    );
    await expect(fetchRecordings()).resolves.toEqual(list);
  });

  it("returns null on a non-ok response (e.g. 503 when storage is disabled)", async () => {
    vi.stubGlobal("fetch", vi.fn().mockResolvedValue({ ok: false }));
    await expect(fetchRecordings()).resolves.toBeNull();
  });

  it("returns null when fetch rejects", async () => {
    vi.stubGlobal("fetch", vi.fn().mockRejectedValue(new Error("network down")));
    await expect(fetchRecordings()).resolves.toBeNull();
  });
});

describe("recordingAudioURL", () => {
  it("builds a relative, same-origin audio URL", () => {
    expect(recordingAudioURL("rec-1")).toBe("api/recordings/rec-1/audio");
  });

  it("URL-encodes the id", () => {
    expect(recordingAudioURL("weird id/../x")).toBe(
      "api/recordings/weird%20id%2F..%2Fx/audio",
    );
  });
});

describe("deleteRecording", () => {
  it("sends a DELETE request and returns true on success", async () => {
    const fetchMock = vi.fn().mockResolvedValue({ ok: true });
    vi.stubGlobal("fetch", fetchMock);

    await expect(deleteRecording("weird id/1")).resolves.toBe(true);
    expect(fetchMock).toHaveBeenCalledWith("api/recordings/weird%20id%2F1", { method: "DELETE" });
  });

  it("returns false on a non-ok response", async () => {
    vi.stubGlobal("fetch", vi.fn().mockResolvedValue({ ok: false }));
    await expect(deleteRecording("rec-1")).resolves.toBe(false);
  });

  it("returns false when fetch rejects", async () => {
    vi.stubGlobal("fetch", vi.fn().mockRejectedValue(new Error("network down")));
    await expect(deleteRecording("rec-1")).resolves.toBe(false);
  });
});
