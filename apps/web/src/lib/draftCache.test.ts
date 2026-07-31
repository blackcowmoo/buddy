/**
 * @vitest-environment jsdom
 */
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { clearDraft, loadDraft, saveDraft } from "./draftCache";

beforeEach(() => {
  localStorage.clear();
});

afterEach(() => {
  vi.useRealTimers();
});

describe("loadDraft", () => {
  it("returns \"\" when nothing is cached for the room", () => {
    expect(loadDraft("room-1")).toBe("");
  });

  it("returns a previously saved draft", () => {
    saveDraft("room-1", "hello there");
    expect(loadDraft("room-1")).toBe("hello there");
  });

  it("keeps drafts for different rooms independent", () => {
    saveDraft("room-1", "from room 1");
    saveDraft("room-2", "from room 2");
    expect(loadDraft("room-1")).toBe("from room 1");
    expect(loadDraft("room-2")).toBe("from room 2");
  });

  it("falls back to \"\" on corrupt JSON", () => {
    localStorage.setItem("buddy.chat.draft.room-1", "{not json");
    expect(loadDraft("room-1")).toBe("");
  });

  it("falls back to \"\" once the ~1 week TTL has passed", () => {
    vi.useFakeTimers();
    vi.setSystemTime(new Date("2026-07-01T00:00:00Z"));
    saveDraft("room-1", "still fresh");
    expect(loadDraft("room-1")).toBe("still fresh");

    vi.setSystemTime(new Date("2026-07-09T00:00:00Z")); // 8 days later
    expect(loadDraft("room-1")).toBe("");
  });
});

describe("saveDraft", () => {
  it("clears the cached draft once emptied", () => {
    saveDraft("room-1", "typing something");
    saveDraft("room-1", "");
    expect(loadDraft("room-1")).toBe("");
    expect(localStorage.getItem("buddy.chat.draft.room-1")).toBeNull();
  });
});

describe("clearDraft", () => {
  it("removes the cached draft for that room only", () => {
    saveDraft("room-1", "keep out of room 2");
    saveDraft("room-2", "unrelated");
    clearDraft("room-1");
    expect(loadDraft("room-1")).toBe("");
    expect(loadDraft("room-2")).toBe("unrelated");
  });
});
