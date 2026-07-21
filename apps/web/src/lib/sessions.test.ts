import { afterEach, describe, expect, it, vi } from "vitest";
import {
  deleteSession,
  fetchSessionCompaction,
  fetchSessionDetail,
  fetchSessions,
} from "./sessions";

afterEach(() => {
  vi.unstubAllGlobals();
});

describe("fetchSessions", () => {
  it("returns the list on a successful response", async () => {
    const sessions = [{ id: "s1", title: "hello", createdAt: 1, updatedAt: 2 }];
    vi.stubGlobal(
      "fetch",
      vi.fn().mockResolvedValue({ ok: true, json: () => Promise.resolve(sessions) }),
    );
    await expect(fetchSessions()).resolves.toEqual(sessions);
  });

  it("returns [] on a non-ok response", async () => {
    vi.stubGlobal("fetch", vi.fn().mockResolvedValue({ ok: false }));
    await expect(fetchSessions()).resolves.toEqual([]);
  });

  it("returns [] when fetch rejects", async () => {
    vi.stubGlobal("fetch", vi.fn().mockRejectedValue(new Error("network down")));
    await expect(fetchSessions()).resolves.toEqual([]);
  });
});

describe("fetchSessionDetail", () => {
  it("returns the detail on a successful response and URL-encodes the id", async () => {
    const detail = {
      session: { id: "s1", title: "hello", createdAt: 1, updatedAt: 2 },
      turns: [{ turn: 1, role: "user" as const, text: "hi", refined: false }],
    };
    const fetchMock = vi.fn().mockResolvedValue({ ok: true, json: () => Promise.resolve(detail) });
    vi.stubGlobal("fetch", fetchMock);

    await expect(fetchSessionDetail("weird id/1")).resolves.toEqual(detail);
    expect(fetchMock).toHaveBeenCalledWith("api/sessions/weird%20id%2F1");
  });

  it("returns null on a non-ok response (e.g. someone else's session id)", async () => {
    vi.stubGlobal("fetch", vi.fn().mockResolvedValue({ ok: false }));
    await expect(fetchSessionDetail("s1")).resolves.toBeNull();
  });

  it("returns null when fetch rejects", async () => {
    vi.stubGlobal("fetch", vi.fn().mockRejectedValue(new Error("network down")));
    await expect(fetchSessionDetail("s1")).resolves.toBeNull();
  });
});

describe("fetchSessionCompaction", () => {
  it("returns the compaction state on a successful response and URL-encodes the id", async () => {
    const compaction = { summary: "likes travel topics", recentMessages: 4, totalTurns: 12 };
    const fetchMock = vi.fn().mockResolvedValue({ ok: true, json: () => Promise.resolve(compaction) });
    vi.stubGlobal("fetch", fetchMock);

    await expect(fetchSessionCompaction("weird id/1")).resolves.toEqual(compaction);
    expect(fetchMock).toHaveBeenCalledWith("api/sessions/weird%20id%2F1/compaction");
  });

  it("returns null on a non-ok response", async () => {
    vi.stubGlobal("fetch", vi.fn().mockResolvedValue({ ok: false }));
    await expect(fetchSessionCompaction("s1")).resolves.toBeNull();
  });

  it("returns null when fetch rejects", async () => {
    vi.stubGlobal("fetch", vi.fn().mockRejectedValue(new Error("network down")));
    await expect(fetchSessionCompaction("s1")).resolves.toBeNull();
  });
});

describe("deleteSession", () => {
  it("sends a DELETE request and returns true on success", async () => {
    const fetchMock = vi.fn().mockResolvedValue({ ok: true });
    vi.stubGlobal("fetch", fetchMock);

    await expect(deleteSession("weird id/1")).resolves.toBe(true);
    expect(fetchMock).toHaveBeenCalledWith("api/sessions/weird%20id%2F1", { method: "DELETE" });
  });

  it("returns false on a non-ok response", async () => {
    vi.stubGlobal("fetch", vi.fn().mockResolvedValue({ ok: false }));
    await expect(deleteSession("s1")).resolves.toBe(false);
  });

  it("returns false when fetch rejects", async () => {
    vi.stubGlobal("fetch", vi.fn().mockRejectedValue(new Error("network down")));
    await expect(deleteSession("s1")).resolves.toBe(false);
  });
});
