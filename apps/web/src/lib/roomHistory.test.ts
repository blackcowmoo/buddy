/**
 * @vitest-environment jsdom
 */
import { beforeEach, describe, expect, it, vi } from "vitest";
import {
  currentRoomHistoryState,
  goBack,
  onRoomPopState,
  parseRoomHash,
  pushRoomState,
  replaceRoomState,
} from "./roomHistory";

beforeEach(() => {
  window.history.replaceState(null, "", "/");
});

describe("parseRoomHash", () => {
  it("parses a room hash into a chat state with its id", () => {
    expect(parseRoomHash("#chat/s1")).toEqual({ view: "chat", id: "s1" });
  });

  it("decodes an encoded id", () => {
    expect(parseRoomHash(`#chat/${encodeURIComponent("a/b")}`)).toEqual({
      view: "chat",
      id: "a/b",
    });
  });

  it("treats the 'new' placeholder as a pending (idless) room", () => {
    expect(parseRoomHash("#chat/new")).toEqual({ view: "chat", id: null });
  });

  it("falls back to list for an empty or unrelated hash", () => {
    expect(parseRoomHash("")).toEqual({ view: "list" });
    expect(parseRoomHash("#recordings")).toEqual({ view: "list" });
  });
});

describe("pushRoomState / replaceRoomState", () => {
  it("push adds a new history entry carrying the room hash and state", () => {
    const before = window.history.length;
    pushRoomState({ view: "chat", id: "s1" });
    expect(window.history.length).toBe(before + 1);
    expect(window.location.hash).toBe("#chat/s1");
    expect(window.history.state).toEqual({ view: "chat", id: "s1" });
  });

  it("push for a pending new room uses the 'new' placeholder hash", () => {
    pushRoomState({ view: "chat", id: null });
    expect(window.location.hash).toBe("#chat/new");
  });

  it("replace swaps the current entry in place without growing the stack", () => {
    pushRoomState({ view: "chat", id: null });
    const before = window.history.length;
    replaceRoomState({ view: "chat", id: "s1" });
    expect(window.history.length).toBe(before);
    expect(window.location.hash).toBe("#chat/s1");
  });

  it("replacing with list clears the hash", () => {
    pushRoomState({ view: "chat", id: "s1" });
    replaceRoomState({ view: "list" });
    expect(window.location.hash).toBe("");
  });
});

describe("currentRoomHistoryState", () => {
  it("reads back a pushed state", () => {
    pushRoomState({ view: "chat", id: "s1" });
    expect(currentRoomHistoryState()).toEqual({ view: "chat", id: "s1" });
  });

  it("falls back to parsing the hash when there is no state (e.g. a fresh full page load)", () => {
    window.history.replaceState(null, "", "/#chat/s1");
    expect(currentRoomHistoryState()).toEqual({ view: "chat", id: "s1" });
  });
});

describe("goBack / onRoomPopState", () => {
  it("reports the state landed on after going back", async () => {
    replaceRoomState({ view: "list" });
    pushRoomState({ view: "chat", id: "s1" });

    const handler = vi.fn();
    const unsubscribe = onRoomPopState(handler);
    goBack();
    await vi.waitFor(() => expect(handler).toHaveBeenCalled());
    expect(handler).toHaveBeenCalledWith({ view: "list" });
    unsubscribe();
  });
});
