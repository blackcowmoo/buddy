import { afterEach, describe, expect, it, vi } from "vitest";
import { fetchJSON, postJSON } from "./fetchJSON";

afterEach(() => {
  vi.unstubAllGlobals();
});

it("fetches relative URLs without changing default request options", async () => {
  const data = { name: "Buddy" };
  const request = vi.fn().mockResolvedValue(Response.json(data));
  vi.stubGlobal("fetch", request);

  await expect(fetchJSON("api/me", null)).resolves.toEqual(data);
  expect(request).toHaveBeenCalledWith("api/me", undefined);
});

it("passes request options through without adding a JSON body or headers", async () => {
  const data = { status: "pending", count: 0 };
  const request = vi.fn().mockResolvedValue(Response.json(data));
  vi.stubGlobal("fetch", request);

  await expect(fetchJSON("api/words/auto-add", null, { method: "POST" })).resolves.toEqual(data);
  expect(request).toHaveBeenCalledWith("api/words/auto-add", { method: "POST" });
});

it("serializes POST data and returns the parsed response", async () => {
  const data = { saved: true };
  const request = vi.fn().mockResolvedValue(Response.json(data));
  vi.stubGlobal("fetch", request);

  await expect(postJSON("api/example", { answer: "hello", optional: undefined }, null)).resolves.toEqual(data);
  expect(request).toHaveBeenCalledWith("api/example", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: '{"answer":"hello"}',
  });
});

describe.each([
  { name: "GET", send: (fallback: object) => fetchJSON("api/example", fallback) },
  { name: "bodyless POST", send: (fallback: object) => fetchJSON("api/example", fallback, { method: "POST" }) },
  { name: "JSON POST", send: (fallback: object) => postJSON("api/example", { answer: "hello" }, fallback) },
])("$name fallback", ({ send }) => {
  it("does not parse an unsuccessful response", async () => {
    const fallback = { failed: true };
    const json = vi.fn();
    vi.stubGlobal("fetch", vi.fn().mockResolvedValue({ ok: false, json }));

    await expect(send(fallback)).resolves.toBe(fallback);
    expect(json).not.toHaveBeenCalled();
  });

  it("returns the fallback when the network request fails", async () => {
    const fallback = { failed: true };
    vi.stubGlobal("fetch", vi.fn().mockRejectedValue(new Error("offline")));

    await expect(send(fallback)).resolves.toBe(fallback);
  });

  it("returns the fallback when a successful response contains invalid JSON", async () => {
    const fallback = { failed: true };
    vi.stubGlobal("fetch", vi.fn().mockResolvedValue(new Response("invalid JSON")));

    await expect(send(fallback)).resolves.toBe(fallback);
  });

  it("preserves a valid null response rather than replacing it with the fallback", async () => {
    vi.stubGlobal("fetch", vi.fn().mockResolvedValue(Response.json(null)));

    await expect(send({ failed: true })).resolves.toBeNull();
  });
});

it("returns the POST fallback without making a request when serialization fails", async () => {
  const body: Record<string, unknown> = {};
  body.self = body;
  const fallback = { failed: true };
  const request = vi.fn();
  vi.stubGlobal("fetch", request);

  await expect(postJSON("api/example", body, fallback)).resolves.toBe(fallback);
  expect(request).not.toHaveBeenCalled();
});
