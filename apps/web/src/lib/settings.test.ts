import { afterEach, describe, expect, it, vi } from "vitest";
import { fetchSettings, saveSettings } from "./settings";

afterEach(() => {
  vi.unstubAllGlobals();
});

describe("fetchSettings", () => {
  it("returns the saved style on a successful response", async () => {
    const settings = {
      interlocutorStyle: "ask interview-style questions",
      learnerProfile: "struggles with articles; loves cooking",
    };
    vi.stubGlobal(
      "fetch",
      vi.fn().mockResolvedValue({ ok: true, json: () => Promise.resolve(settings) }),
    );
    await expect(fetchSettings()).resolves.toEqual(settings);
  });

  it("returns null on a non-ok response, distinct from a genuinely empty style", async () => {
    vi.stubGlobal("fetch", vi.fn().mockResolvedValue({ ok: false }));
    await expect(fetchSettings()).resolves.toBeNull();
  });

  it("returns null when fetch rejects", async () => {
    vi.stubGlobal("fetch", vi.fn().mockRejectedValue(new Error("network down")));
    await expect(fetchSettings()).resolves.toBeNull();
  });
});

describe("saveSettings", () => {
  it("sends a PUT request with the style and returns true on success", async () => {
    const fetchMock = vi.fn().mockResolvedValue({ ok: true });
    vi.stubGlobal("fetch", fetchMock);

    await expect(saveSettings("sound professional")).resolves.toBe(true);
    expect(fetchMock).toHaveBeenCalledWith("api/settings", {
      method: "PUT",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ interlocutorStyle: "sound professional" }),
    });
  });

  it("returns false on a non-ok response", async () => {
    vi.stubGlobal("fetch", vi.fn().mockResolvedValue({ ok: false }));
    await expect(saveSettings("x")).resolves.toBe(false);
  });

  it("returns false when fetch rejects", async () => {
    vi.stubGlobal("fetch", vi.fn().mockRejectedValue(new Error("network down")));
    await expect(saveSettings("x")).resolves.toBe(false);
  });
});
