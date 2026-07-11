import { afterEach, describe, expect, it, vi } from "vitest";
import { fetchMe } from "./me";

afterEach(() => {
  vi.unstubAllGlobals();
});

describe("fetchMe", () => {
  it("returns the identity on a successful response", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn().mockResolvedValue({
        ok: true,
        json: () => Promise.resolve({ identityMode: "oidc", id: "alex@example.com" }),
      }),
    );
    await expect(fetchMe()).resolves.toEqual({ identityMode: "oidc", id: "alex@example.com" });
  });

  it("returns null on a non-ok response", async () => {
    vi.stubGlobal("fetch", vi.fn().mockResolvedValue({ ok: false }));
    await expect(fetchMe()).resolves.toBeNull();
  });

  it("returns null when fetch rejects", async () => {
    vi.stubGlobal("fetch", vi.fn().mockRejectedValue(new Error("network down")));
    await expect(fetchMe()).resolves.toBeNull();
  });
});
