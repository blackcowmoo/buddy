/**
 * @vitest-environment jsdom
 */
import "@testing-library/jest-dom/vitest";
import { act, cleanup, fireEvent, render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import type { ServerEvent } from "./lib/protocol";

let capturedOnEvent: ((e: ServerEvent) => void) | null = null;

// Mirrors lib/roomHistory's RoomHistoryState — mocked below so these tests
// assert on the calls App.tsx makes into that module instead of fighting
// jsdom's location/history plumbing (see lib/roomHistory.test.ts for real
// coverage of the module itself).
type MockRoomState = { view: "list" } | { view: "chat"; id: string | null };
let capturedPopStateHandler: ((s: MockRoomState) => void) | null = null;

vi.mock("./lib/ws", () => ({
  BuddyClient: vi.fn().mockImplementation(function BuddyClient(
    this: object,
    onEvent: (e: ServerEvent) => void,
  ) {
    capturedOnEvent = onEvent;
    return Object.assign(this, {
      connect: vi.fn(),
      close: vi.fn(),
      sendText: vi.fn(),
      sendAudio: vi.fn(),
    });
  }),
}));

vi.mock("./audio/recorder", () => ({
  PCMRecorder: vi.fn().mockImplementation(function PCMRecorder(this: object) {
    return Object.assign(this, {
      isRecording: false,
      start: vi.fn(),
      stop: vi.fn(),
    });
  }),
}));

vi.mock("./tts/kokoro", () => ({
  KokoroSpeaker: vi.fn().mockImplementation(function KokoroSpeaker(this: object) {
    return Object.assign(this, {
      loaded: false,
      load: vi.fn(),
      speak: vi.fn(),
    });
  }),
}));

vi.mock("./lib/me", () => ({
  fetchMe: vi.fn(),
}));

vi.mock("./lib/sessions", () => ({
  fetchSessions: vi.fn(),
  fetchSessionDetail: vi.fn(),
  fetchSessionCompaction: vi.fn(),
  deleteSession: vi.fn(),
}));

vi.mock("./lib/roomHistory", () => ({
  parseRoomHash: vi.fn(() => ({ view: "list" })),
  currentRoomHistoryState: vi.fn(() => ({ view: "list" })),
  pushRoomState: vi.fn(),
  replaceRoomState: vi.fn(),
  goBack: vi.fn(),
  onRoomPopState: vi.fn((handler: (s: MockRoomState) => void) => {
    capturedPopStateHandler = handler;
    return vi.fn();
  }),
}));

import { App } from "./App";
import { fetchMe } from "./lib/me";
import {
  currentRoomHistoryState,
  goBack,
  parseRoomHash,
  pushRoomState,
  replaceRoomState,
} from "./lib/roomHistory";
import {
  deleteSession,
  fetchSessionCompaction,
  fetchSessionDetail,
  fetchSessions,
} from "./lib/sessions";
import { KokoroSpeaker } from "./tts/kokoro";
import { BuddyClient } from "./lib/ws";

beforeEach(() => {
  localStorage.clear();
  capturedOnEvent = null;
  capturedPopStateHandler = null;
  vi.mocked(fetchMe).mockResolvedValue(null);
  vi.mocked(fetchSessions).mockResolvedValue([]);
  vi.mocked(fetchSessionDetail).mockResolvedValue(null);
  vi.mocked(fetchSessionCompaction).mockResolvedValue(null);
  vi.mocked(parseRoomHash).mockReturnValue({ view: "list" });
  vi.mocked(currentRoomHistoryState).mockReturnValue({ view: "list" });
  vi.stubGlobal("location", {
    protocol: "http:",
    host: "buddy.example",
    pathname: "/",
    hash: "",
    search: "",
    assign: vi.fn(),
  });
  localStorage.clear();
  document.documentElement.removeAttribute("data-theme");
  // jsdom has no matchMedia implementation; App applies the theme on mount.
  vi.stubGlobal(
    "matchMedia",
    vi.fn().mockImplementation((query: string) => ({
      matches: false,
      media: query,
      addEventListener: vi.fn(),
      removeEventListener: vi.fn(),
    })),
  );
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  vi.clearAllMocks();
});

function openMenu(user: ReturnType<typeof userEvent.setup>) {
  return user.click(screen.getByRole("button", { name: "Menu" }));
}

// Play-rate buttons live behind a per-message study popover, not inline —
// open it before a test tries to find/click one of the rate buttons.
async function openStudyPopover(user: ReturnType<typeof userEvent.setup>) {
  await user.click(await screen.findByRole("button", { name: "발음 연습 열기" }));
}

// Grammar feedback lives behind a per-message popover too — open it (once
// the check has finished, so the button isn't disabled/spinning) before a
// test asserts on the correction content.
async function openGrammarPopover(user: ReturnType<typeof userEvent.setup>) {
  await user.click(await screen.findByRole("button", { name: /^문법 피드백 열기/ }));
}

// The app always lands on the room list, and the room list has its own
// hamburger menu (identity, theme, PR nav). Tests for chat-only menu items
// (voice/playback speed, reset) need to get into a room first, the same way
// a learner would: start a new chat.
async function enterNewChat(user: ReturnType<typeof userEvent.setup>) {
  await user.click(screen.getByRole("button", { name: "+ 새 대화" }));
  await screen.findByRole("button", { name: "Menu" });
}

function lastClientInstance() {
  const mocked = vi.mocked(BuddyClient);
  return mocked.mock.instances[mocked.mock.instances.length - 1] as unknown as {
    connect: ReturnType<typeof vi.fn>;
    close: ReturnType<typeof vi.fn>;
  };
}

/** Simulates a server event arriving over the (mocked) websocket. */
function emit(e: ServerEvent) {
  capturedOnEvent?.(e);
}

// With fake timers installed, a promise-chain-driven re-render (e.g. a
// fetchSessionDetail().then(setMsgs) continuation) needs its scheduler
// macrotask advanced by hand — advanceTimersByTimeAsync(0) runs whatever's
// due "now", but a render that takes a couple of microtask/timer hops to
// land can outrun a single call, so this repeats it a few times to be safe.
async function flushRenders() {
  for (let i = 0; i < 5; i++) {
    await vi.advanceTimersByTimeAsync(0);
  }
}

describe("room list", () => {
  it("is the initial view — no WS connection until a room is opened", () => {
    render(<App />);
    expect(screen.getByRole("button", { name: "+ 새 대화" })).toBeInTheDocument();
    expect(vi.mocked(BuddyClient).mock.instances[0]).toBeDefined();
    expect(lastClientInstance().connect).not.toHaveBeenCalled();
  });

  it("shows an empty state with no sessions", async () => {
    render(<App />);
    expect(await screen.findByText(/아직 대화 기록이 없어요/)).toBeInTheDocument();
  });

  it("lists sessions returned by the server", async () => {
    vi.mocked(fetchSessions).mockResolvedValue([
      { id: "s1", title: "hello there", createdAt: 1, updatedAt: Math.floor(Date.now() / 1000) },
    ]);
    render(<App />);
    expect(await screen.findByText("hello there")).toBeInTheDocument();
  });

  it("starting a new chat opens the WS with no session id", async () => {
    const user = userEvent.setup();
    render(<App />);
    await enterNewChat(user);
    expect(lastClientInstance().connect).toHaveBeenCalledWith(undefined);
  });

  it("clicking a session hydrates its transcript and resumes it by id", async () => {
    vi.mocked(fetchSessions).mockResolvedValue([
      { id: "s1", title: "hello there", createdAt: 1, updatedAt: 2 },
    ]);
    vi.mocked(fetchSessionDetail).mockResolvedValue({
      hasMore: false,
      session: { id: "s1", title: "hello there", createdAt: 1, updatedAt: 2 },
      turns: [
        { turn: 1, role: "user", text: "hi", refined: false },
        { turn: 1, role: "assistant", text: "hello!", refined: false },
      ],
    });
    const user = userEvent.setup();
    render(<App />);
    await user.click(await screen.findByText("hello there"));

    expect(await screen.findByText("hi")).toBeInTheDocument();
    expect(await screen.findByText("hello!")).toBeInTheDocument();
    expect(fetchSessionDetail).toHaveBeenCalledWith("s1", { limit: 30 });
    expect(lastClientInstance().connect).toHaveBeenCalledWith("s1");
  });

  it("scrolls the transcript to the bottom when entering a room, so the latest turn is visible", async () => {
    // jsdom never computes real layout, so .scrollHeight is always 0 — stub
    // it to a value that would be "scrolled up" by default, and confirm
    // App.tsx actively drives .scrollTop to match it on entry rather than
    // leaving the view wherever it happened to render (the reported bug:
    // reopening a room landed on the oldest turn, not the latest).
    const scrollHeightSpy = vi
      .spyOn(HTMLElement.prototype, "scrollHeight", "get")
      .mockReturnValue(2000);
    vi.mocked(fetchSessions).mockResolvedValue([
      { id: "s1", title: "hello there", createdAt: 1, updatedAt: 2 },
    ]);
    vi.mocked(fetchSessionDetail).mockResolvedValue({
      hasMore: false,
      session: { id: "s1", title: "hello there", createdAt: 1, updatedAt: 2 },
      turns: [{ turn: 1, role: "user", text: "hi", refined: false }],
    });
    const user = userEvent.setup();
    render(<App />);
    await user.click(await screen.findByText("hello there"));
    expect(await screen.findByText("hi")).toBeInTheDocument();

    const convo = document.querySelector(".convo");
    if (!convo) throw new Error("expected .convo container to be rendered");
    expect((convo as HTMLElement).scrollTop).toBe(2000);

    scrollHeightSpy.mockRestore();
  });

  it("scrolling near the top of a room with more history loads and prepends an older page", async () => {
    vi.mocked(fetchSessions).mockResolvedValue([
      { id: "s1", title: "hello there", createdAt: 1, updatedAt: 2 },
    ]);
    vi.mocked(fetchSessionDetail).mockResolvedValueOnce({
      hasMore: true,
      session: { id: "s1", title: "hello there", createdAt: 1, updatedAt: 2 },
      turns: [
        { turn: 5, role: "user", text: "recent hi", refined: false },
        { turn: 5, role: "assistant", text: "recent hello", refined: false },
      ],
    });
    const user = userEvent.setup();
    render(<App />);
    await user.click(await screen.findByText("hello there"));
    expect(await screen.findByText("recent hi")).toBeInTheDocument();

    vi.mocked(fetchSessionDetail).mockResolvedValueOnce({
      hasMore: false,
      session: { id: "s1", title: "hello there", createdAt: 1, updatedAt: 2 },
      turns: [
        { turn: 4, role: "user", text: "older hi", refined: false },
        { turn: 4, role: "assistant", text: "older hello", refined: false },
      ],
    });
    const convo = document.querySelector(".convo");
    if (!convo) throw new Error("expected .convo container to be rendered");
    fireEvent.scroll(convo);

    expect(await screen.findByText("older hi")).toBeInTheDocument();
    expect(fetchSessionDetail).toHaveBeenLastCalledWith("s1", { before: 5, limit: 30 });

    // Prepended, not appended: the older turn reads before the recent one.
    const rows = Array.from(convo.querySelectorAll(".bubble")).map((n) => n.textContent);
    expect(rows.indexOf("older hi")).toBeLessThan(rows.indexOf("recent hi"));
  });

  it("does not request another page on scroll once hasMore is false", async () => {
    vi.mocked(fetchSessions).mockResolvedValue([
      { id: "s1", title: "hello there", createdAt: 1, updatedAt: 2 },
    ]);
    vi.mocked(fetchSessionDetail).mockResolvedValue({
      hasMore: false,
      session: { id: "s1", title: "hello there", createdAt: 1, updatedAt: 2 },
      turns: [{ turn: 1, role: "user", text: "hi", refined: false }],
    });
    const user = userEvent.setup();
    render(<App />);
    await user.click(await screen.findByText("hello there"));
    expect(await screen.findByText("hi")).toBeInTheDocument();

    vi.mocked(fetchSessionDetail).mockClear();
    const convo = document.querySelector(".convo");
    if (!convo) throw new Error("expected .convo container to be rendered");
    fireEvent.scroll(convo);
    await Promise.resolve();

    expect(fetchSessionDetail).not.toHaveBeenCalled();
  });

  it("asks for confirmation, deletes, and removes the row on confirmed success", async () => {
    vi.mocked(fetchSessions).mockResolvedValue([
      { id: "s1", title: "hello there", createdAt: 1, updatedAt: 2 },
    ]);
    vi.mocked(deleteSession).mockResolvedValue(true);
    const confirmSpy = vi.spyOn(window, "confirm").mockReturnValue(true);
    const user = userEvent.setup();
    render(<App />);

    await user.click(await screen.findByRole("button", { name: "대화 삭제" }));

    expect(confirmSpy).toHaveBeenCalled();
    expect(deleteSession).toHaveBeenCalledWith("s1");
    expect(await screen.findByText(/아직 대화 기록이 없어요/)).toBeInTheDocument();
  });

  it("does not delete a session when the confirmation is declined", async () => {
    vi.mocked(fetchSessions).mockResolvedValue([
      { id: "s1", title: "hello there", createdAt: 1, updatedAt: 2 },
    ]);
    vi.spyOn(window, "confirm").mockReturnValue(false);
    const user = userEvent.setup();
    render(<App />);

    await user.click(await screen.findByRole("button", { name: "대화 삭제" }));

    expect(deleteSession).not.toHaveBeenCalled();
    expect(screen.getByText("hello there")).toBeInTheDocument();
  });

  it("keeps the session in the list when the delete request fails", async () => {
    vi.mocked(fetchSessions).mockResolvedValue([
      { id: "s1", title: "hello there", createdAt: 1, updatedAt: 2 },
    ]);
    vi.mocked(deleteSession).mockResolvedValue(false);
    vi.spyOn(window, "confirm").mockReturnValue(true);
    const user = userEvent.setup();
    render(<App />);

    await user.click(await screen.findByRole("button", { name: "대화 삭제" }));

    expect(deleteSession).toHaveBeenCalledWith("s1");
    expect(screen.getByText("hello there")).toBeInTheDocument();
  });

  it('"back to list" reuses the room\'s history entry, which closes the connection and re-shows the room list', async () => {
    const user = userEvent.setup();
    render(<App />);
    await enterNewChat(user);

    await user.click(screen.getByRole("button", { name: "목록으로" }));
    expect(goBack).toHaveBeenCalled();
    // goBack() is history.back() — a real browser resolves that
    // asynchronously via popstate, which App's mount effect listens for.
    act(() => capturedPopStateHandler?.({ view: "list" }));

    expect(await screen.findByRole("button", { name: "+ 새 대화" })).toBeInTheDocument();
    expect(lastClientInstance().close).toHaveBeenCalled();
  });

  it("has a menu button reachable without opening a room first", async () => {
    const user = userEvent.setup();
    render(<App />);
    await openMenu(user);
    expect(screen.getByRole("menu")).toBeInTheDocument();
  });

  it("shows identity, theme switch, and PR nav in the list menu, but no chat-only items", async () => {
    vi.mocked(fetchMe).mockResolvedValue({ identityMode: "oidc", id: "alex@example.com" });
    const user = userEvent.setup();
    render(<App />);
    await openMenu(user);
    expect(await screen.findByText("alex@example.com")).toBeInTheDocument();
    expect(screen.getByRole("group", { name: "테마" })).toBeInTheDocument();
    expect(screen.getByLabelText("PR 미리보기로 이동")).toBeInTheDocument();
    expect(screen.queryByRole("menuitem", { name: /대화 초기화/ })).not.toBeInTheDocument();
    expect(screen.queryByText("음성 활성화")).not.toBeInTheDocument();
  });

  it("closes the list menu when a room is opened", async () => {
    const user = userEvent.setup();
    render(<App />);
    await openMenu(user);
    expect(screen.getByRole("menu")).toBeInTheDocument();
    await user.click(screen.getByRole("button", { name: "+ 새 대화" }));
    expect(screen.queryByRole("menu")).not.toBeInTheDocument();
  });
});

describe("browser history", () => {
  it("pushes a history entry for a brand-new room", async () => {
    const user = userEvent.setup();
    render(<App />);
    await enterNewChat(user);
    expect(pushRoomState).toHaveBeenCalledWith({ view: "chat", id: null });
  });

  it("pushes a history entry carrying the id when opening an existing room", async () => {
    vi.mocked(fetchSessions).mockResolvedValue([
      { id: "s1", title: "hello there", createdAt: 1, updatedAt: 2 },
    ]);
    const user = userEvent.setup();
    render(<App />);
    await user.click(await screen.findByText("hello there"));
    expect(pushRoomState).toHaveBeenCalledWith({ view: "chat", id: "s1" });
  });

  it("upgrades a new room's pending history entry once the server assigns its id", async () => {
    vi.mocked(currentRoomHistoryState).mockReturnValue({ view: "chat", id: null });
    const user = userEvent.setup();
    render(<App />);
    await enterNewChat(user);

    act(() => emit({ type: "ready", turn: 0, session: "s9" }));

    expect(replaceRoomState).toHaveBeenCalledWith({ view: "chat", id: "s9" });
  });

  it("does not touch history on 'ready' once the room already has a known id", async () => {
    vi.mocked(fetchSessions).mockResolvedValue([
      { id: "s1", title: "hello there", createdAt: 1, updatedAt: 2 },
    ]);
    vi.mocked(currentRoomHistoryState).mockReturnValue({ view: "chat", id: "s1" });
    const user = userEvent.setup();
    render(<App />);
    await user.click(await screen.findByText("hello there"));
    vi.mocked(replaceRoomState).mockClear();

    act(() => emit({ type: "ready", turn: 0, session: "s1" }));

    expect(replaceRoomState).not.toHaveBeenCalled();
  });

  it("restores an open room from the URL hash on mount, e.g. after a refresh", async () => {
    vi.mocked(parseRoomHash).mockReturnValue({ view: "chat", id: "s1" });
    vi.mocked(fetchSessionDetail).mockResolvedValue({
      hasMore: false,
      session: { id: "s1", title: "hello there", createdAt: 1, updatedAt: 2 },
      turns: [{ turn: 1, role: "user", text: "hi", refined: false }],
    });
    render(<App />);

    expect(await screen.findByText("hi")).toBeInTheDocument();
    expect(fetchSessionDetail).toHaveBeenCalledWith("s1", { limit: 30 });
    expect(lastClientInstance().connect).toHaveBeenCalledWith("s1");
  });

  it("falls back to the list and clears the hash when the restored room no longer exists", async () => {
    vi.mocked(parseRoomHash).mockReturnValue({ view: "chat", id: "gone" });
    vi.mocked(fetchSessionDetail).mockResolvedValue(null);
    render(<App />);

    expect(await screen.findByRole("button", { name: "+ 새 대화" })).toBeInTheDocument();
    expect(replaceRoomState).toHaveBeenCalledWith({ view: "list" });
  });

  it("a popstate back into the list resets the open room, same as the in-app back button", async () => {
    const user = userEvent.setup();
    render(<App />);
    await enterNewChat(user);
    act(() => emit({ type: "assistant_done", turn: 0, text: "Hey there!" }));

    act(() => capturedPopStateHandler?.({ view: "list" }));

    expect(await screen.findByRole("button", { name: "+ 새 대화" })).toBeInTheDocument();
    expect(lastClientInstance().close).toHaveBeenCalled();
  });

  it("a popstate forward into a room hydrates and reconnects it", async () => {
    vi.mocked(fetchSessionDetail).mockResolvedValue({
      hasMore: false,
      session: { id: "s1", title: "hello there", createdAt: 1, updatedAt: 2 },
      turns: [{ turn: 1, role: "user", text: "hi", refined: false }],
    });
    render(<App />);
    await screen.findByRole("button", { name: "+ 새 대화" });

    act(() => capturedPopStateHandler?.({ view: "chat", id: "s1" }));

    expect(await screen.findByText("hi")).toBeInTheDocument();
    expect(lastClientInstance().connect).toHaveBeenCalledWith("s1");
  });
});

describe("hamburger menu", () => {
  it("is closed by default", async () => {
    const user = userEvent.setup();
    render(<App />);
    await enterNewChat(user);
    expect(screen.queryByRole("menu")).not.toBeInTheDocument();
  });

  it("opens on click and closes on a second click", async () => {
    const user = userEvent.setup();
    render(<App />);
    await enterNewChat(user);
    await openMenu(user);
    expect(screen.getByRole("menu")).toBeInTheDocument();
    await openMenu(user);
    expect(screen.queryByRole("menu")).not.toBeInTheDocument();
  });

  it("closes when clicking outside the menu", async () => {
    const user = userEvent.setup();
    render(<App />);
    await enterNewChat(user);
    await openMenu(user);
    expect(screen.getByRole("menu")).toBeInTheDocument();
    await user.click(document.body);
    expect(screen.queryByRole("menu")).not.toBeInTheDocument();
  });

  it("closes on Escape", async () => {
    const user = userEvent.setup();
    render(<App />);
    await enterNewChat(user);
    await openMenu(user);
    expect(screen.getByRole("menu")).toBeInTheDocument();
    await user.keyboard("{Escape}");
    expect(screen.queryByRole("menu")).not.toBeInTheDocument();
  });

  it("shows the oidc email once fetched", async () => {
    vi.mocked(fetchMe).mockResolvedValue({ identityMode: "oidc", id: "alex@example.com" });
    const user = userEvent.setup();
    render(<App />);
    await enterNewChat(user);
    await openMenu(user);
    expect(await screen.findByText("alex@example.com")).toBeInTheDocument();
  });

  it("shows an anonymous label when there is no real identity", async () => {
    const user = userEvent.setup();
    render(<App />);
    await enterNewChat(user);
    await openMenu(user);
    expect(await screen.findByText("익명 사용자")).toBeInTheDocument();
  });

  it("rejects a non-numeric PR path and does not navigate", async () => {
    const user = userEvent.setup();
    render(<App />);
    await enterNewChat(user);
    await openMenu(user);
    await user.type(screen.getByLabelText("PR 미리보기로 이동"), "abc");
    await user.click(screen.getByRole("button", { name: "이동" }));
    expect(await screen.findByText(/숫자만 입력하세요/)).toBeInTheDocument();
    expect(location.assign).not.toHaveBeenCalled();
  });

  it("navigates to /pr/<n>/ for a numeric PR input", async () => {
    const user = userEvent.setup();
    render(<App />);
    await enterNewChat(user);
    await openMenu(user);
    await user.type(screen.getByLabelText("PR 미리보기로 이동"), "14");
    await user.click(screen.getByRole("button", { name: "이동" }));
    expect(location.assign).toHaveBeenCalledWith("/pr/14/");
  });

  it("navigates to the relative recordings page from the menu", async () => {
    const user = userEvent.setup();
    render(<App />);
    await openMenu(user);
    await user.click(screen.getByRole("menuitem", { name: /녹음 목록/ }));
    expect(location.assign).toHaveBeenCalledWith("recordings");
  });
});

describe("theme switch", () => {
  it("defaults to following the system theme", async () => {
    const user = userEvent.setup();
    render(<App />);
    await enterNewChat(user);
    await openMenu(user);
    expect(screen.getByRole("button", { name: "시스템 설정" })).toHaveAttribute(
      "aria-pressed",
      "true",
    );
    expect(document.documentElement.getAttribute("data-theme")).toBe("dark");
  });

  it("switches to white, applies it, and persists it to localStorage only", async () => {
    const user = userEvent.setup();
    render(<App />);
    await enterNewChat(user);
    await openMenu(user);
    await user.click(screen.getByRole("button", { name: "화이트" }));
    expect(document.documentElement.getAttribute("data-theme")).toBe("white");
    expect(localStorage.getItem("buddy-theme")).toBe("white");
  });

  it("restores a previously chosen theme on mount", async () => {
    localStorage.setItem("buddy-theme", "dark");
    const user = userEvent.setup();
    render(<App />);
    await enterNewChat(user);
    await openMenu(user);
    expect(screen.getByRole("button", { name: "다크" })).toHaveAttribute("aria-pressed", "true");
    expect(document.documentElement.getAttribute("data-theme")).toBe("dark");
  });
});

describe("correction cards", () => {
  it("labels a context-type issue in Korean on the user's turn", async () => {
    const user = userEvent.setup();
    render(<App />);
    await enterNewChat(user);
    act(() => emit({ type: "final_transcript", turn: 1, text: "I are fine." }));
    act(() =>
      emit({
        type: "correction",
        turn: 1,
        correction: {
          original: "I are fine.",
          corrected: "I am fine.",
          issues: [
            {
              type: "context",
              span: "I are",
              suggestion: "I am",
              explanation: "Doesn't match the earlier context.",
              explanationTranslation: "앞 문맥과 맞지 않아요",
            },
          ],
        },
      }),
    );
    await openGrammarPopover(user);
    expect(await screen.findByText("문맥")).toBeInTheDocument();
    expect(screen.getByText("Doesn't match the earlier context.")).toBeInTheDocument();
    expect(screen.getByText("앞 문맥과 맞지 않아요")).toBeInTheDocument();
  });
});

describe("input source indicator", () => {
  it("shows a mic icon on a message transcribed from voice", async () => {
    const user = userEvent.setup();
    render(<App />);
    await enterNewChat(user);
    act(() => emit({ type: "final_transcript", turn: 1, text: "I are fine.", source: "voice" }));
    expect(await screen.findByTitle("음성으로 입력함")).toBeInTheDocument();
  });

  it("shows a keyboard icon on a typed message", async () => {
    const user = userEvent.setup();
    render(<App />);
    await enterNewChat(user);
    act(() => emit({ type: "final_transcript", turn: 1, text: "I am fine.", source: "text" }));
    expect(await screen.findByTitle("채팅으로 입력함")).toBeInTheDocument();
  });

  it("carries the source over when a voice turn is later upgraded by refine", async () => {
    const user = userEvent.setup();
    render(<App />);
    await enterNewChat(user);
    act(() => emit({ type: "final_transcript", turn: 1, text: "i are hungry", source: "voice" }));
    act(() => emit({ type: "refined_transcript", turn: 1, text: "I am hungry" }));
    await screen.findByText("I am hungry");
    expect(screen.getByTitle("음성으로 입력함")).toBeInTheDocument();
  });

  it("restores the source of a hydrated turn from history", async () => {
    vi.mocked(fetchSessions).mockResolvedValue([
      { id: "s1", title: "hello there", createdAt: 1, updatedAt: 2 },
    ]);
    vi.mocked(fetchSessionDetail).mockResolvedValue({
      hasMore: false,
      session: { id: "s1", title: "hello there", createdAt: 1, updatedAt: 2 },
      turns: [{ turn: 1, role: "user", text: "hi", refined: false, source: "voice" }],
    });
    const user = userEvent.setup();
    render(<App />);
    await user.click(await screen.findByText("hello there"));

    expect(await screen.findByTitle("음성으로 입력함")).toBeInTheDocument();
  });
});

describe("grammar feedback button", () => {
  it("shows a spinner while the grammar check is running", async () => {
    const user = userEvent.setup();
    render(<App />);
    await enterNewChat(user);
    act(() => emit({ type: "final_transcript", turn: 1, text: "I are fine." }));
    expect(await screen.findByRole("button", { name: "문법 확인 중" })).toBeDisabled();
  });

  it("shows a clean-sentence message when the check finds no issues", async () => {
    const user = userEvent.setup();
    render(<App />);
    await enterNewChat(user);
    act(() => emit({ type: "final_transcript", turn: 1, text: "I am fine." }));
    act(() =>
      emit({
        type: "correction",
        turn: 1,
        correction: { original: "I am fine.", corrected: "I am fine.", issues: [] },
      }),
    );
    await user.click(await screen.findByRole("button", { name: "문법 피드백 열기 (문제 없음)" }));
    expect(await screen.findByText("문법 문제가 없어요 👍")).toBeInTheDocument();
  });

  // A session with no recent activity (updatedAt long in the past) is
  // treated as permanently uncorrected rather than still in flight — a
  // missing correction here is most likely a room predating the feature, or
  // one whose correct() call failed with nothing left to retry it.
  it("does not show a spinner for a hydrated turn with no saved correction in a stale session", async () => {
    vi.mocked(fetchSessions).mockResolvedValue([
      { id: "s1", title: "hello there", createdAt: 1, updatedAt: 2 },
    ]);
    vi.mocked(fetchSessionDetail).mockResolvedValue({
      hasMore: false,
      session: { id: "s1", title: "hello there", createdAt: 1, updatedAt: 2 },
      turns: [{ turn: 1, role: "user", text: "hi", refined: false }],
    });
    const user = userEvent.setup();
    render(<App />);
    await user.click(await screen.findByText("hello there"));

    await screen.findByText("hi");
    expect(screen.queryByRole("button", { name: /^문법/ })).not.toBeInTheDocument();
  });

  // Guards a real reported bug: leaving a room while correct() was still
  // running for the last turn, then reopening it, used to silently drop the
  // hourglass (a hydrated turn with no saved correction rendered nothing at
  // all, permanently, unlike the translation spinner which already survived
  // reopening). correct() runs detached from the connection (see
  // pipeline.HandleText) so it keeps going and persists after the learner
  // leaves — a missing correction on a turn from a recently-active session
  // now shows the same spinner instead, and the app polls until it lands.
  it("shows a grammar spinner for a hydrated turn whose correction is still in flight", async () => {
    vi.mocked(fetchSessions).mockResolvedValue([
      { id: "s1", title: "hello there", createdAt: 1, updatedAt: Math.floor(Date.now() / 1000) },
    ]);
    vi.mocked(fetchSessionDetail).mockResolvedValue({
      hasMore: false,
      session: { id: "s1", title: "hello there", createdAt: 1, updatedAt: Math.floor(Date.now() / 1000) },
      turns: [{ turn: 1, role: "user", text: "hi", refined: false }],
    });
    const user = userEvent.setup();
    render(<App />);
    await user.click(await screen.findByText("hello there"));

    await screen.findByText("hi");
    expect(await screen.findByRole("button", { name: "문법 확인 중" })).toBeDisabled();
  });

  // Guards the fix for the actual reported confusion: a failed analysis pass
  // used to just never emit an event, so the spinner hung forever with no
  // way to tell it apart from "still checking" or, once the turn aged out of
  // the frontend's recently-active window, from "already correct" (both
  // rendered nothing). Failed must now be its own distinct, visible state.
  it("shows a distinct failed state when the analysis pass itself errors", async () => {
    const user = userEvent.setup();
    render(<App />);
    await enterNewChat(user);
    act(() => emit({ type: "final_transcript", turn: 1, text: "I are fine." }));
    act(() => emit({ type: "correction", turn: 1, failed: true }));

    const btn = await screen.findByRole("button", {
      name: "문법 피드백 열기 (확인 실패, 자동으로 다시 시도해요)",
    });
    expect(btn).not.toBeDisabled();
    await user.click(btn);
    expect(await screen.findByText(/문법 확인에 실패했어요/)).toBeInTheDocument();
  });

  // Unlike the old ambiguous "no correction saved" case (still hidden for a
  // stale session, see the test above), a durably-recorded failure is shown
  // regardless of how long ago the session was active — the backend already
  // knows for a fact the job failed, so there's nothing left to guess at
  // (see store.Turn.CorrectionStatus).
  it("shows the failed state for a hydrated turn with a durably-failed correction job, even in a stale session", async () => {
    vi.mocked(fetchSessions).mockResolvedValue([
      { id: "s1", title: "hello there", createdAt: 1, updatedAt: 2 },
    ]);
    vi.mocked(fetchSessionDetail).mockResolvedValue({
      hasMore: false,
      session: { id: "s1", title: "hello there", createdAt: 1, updatedAt: 2 },
      turns: [{ turn: 1, role: "user", text: "hi", refined: false, correctionStatus: "failed" }],
    });
    const user = userEvent.setup();
    render(<App />);
    await user.click(await screen.findByText("hello there"));

    await screen.findByText("hi");
    expect(
      await screen.findByRole("button", { name: "문법 피드백 열기 (확인 실패, 자동으로 다시 시도해요)" }),
    ).toBeInTheDocument();
  });
});

// Session-wide feedback list button, next to the ☰ menu in the chat header —
// lists every user turn's feedback collected so far (see FeedbackSummary).
describe("feedback summary", () => {
  function openFeedbackPanel(user: ReturnType<typeof userEvent.setup>) {
    return user.click(screen.getByRole("button", { name: "피드백 모아보기" }));
  }

  it("does not appear on the room list", async () => {
    render(<App />);
    await screen.findByRole("button", { name: "Menu" });
    expect(screen.queryByRole("button", { name: "피드백 모아보기" })).not.toBeInTheDocument();
  });

  it("shows an empty state before any feedback has arrived", async () => {
    const user = userEvent.setup();
    render(<App />);
    await enterNewChat(user);
    await openFeedbackPanel(user);
    expect(await screen.findByText("아직 피드백이 없어요 👍")).toBeInTheDocument();
  });

  it("lists accumulated feedback across multiple turns and excludes clean ones", async () => {
    const user = userEvent.setup();
    render(<App />);
    await enterNewChat(user);

    act(() => emit({ type: "final_transcript", turn: 1, text: "I are fine." }));
    act(() =>
      emit({
        type: "correction",
        turn: 1,
        correction: {
          original: "I are fine.",
          corrected: "I am fine.",
          issues: [
            {
              type: "grammar",
              span: "I are",
              suggestion: "I am",
              explanation: "Subject-verb disagreement.",
              explanationTranslation: "주어-동사 불일치",
            },
          ],
        },
      }),
    );
    act(() => emit({ type: "final_transcript", turn: 2, text: "I am fine." }));
    act(() =>
      emit({
        type: "correction",
        turn: 2,
        correction: { original: "I am fine.", corrected: "I am fine.", issues: [] },
      }),
    );
    act(() => emit({ type: "final_transcript", turn: 3, text: "She go home." }));
    act(() =>
      emit({
        type: "correction",
        turn: 3,
        correction: {
          original: "She go home.",
          corrected: "She goes home.",
          issues: [
            {
              type: "grammar",
              span: "go",
              suggestion: "goes",
              explanation: "Third-person singular present tense.",
              explanationTranslation: "3인칭 단수 현재형",
            },
          ],
        },
      }),
    );

    await openFeedbackPanel(user);
    expect(await screen.findByText("지금까지 2개 메시지에 피드백이 있어요")).toBeInTheDocument();
    expect(screen.getByText("Subject-verb disagreement.")).toBeInTheDocument();
    expect(screen.getByText("주어-동사 불일치")).toBeInTheDocument();
    expect(screen.getByText("Third-person singular present tense.")).toBeInTheDocument();
    expect(screen.getByText("3인칭 단수 현재형")).toBeInTheDocument();
    expect(screen.queryByText("I am fine.", { selector: ".feedback-original" })).not.toBeInTheDocument();
  });

  it("shows hydrated feedback from a reopened room without a live event", async () => {
    vi.mocked(fetchSessions).mockResolvedValue([
      { id: "s1", title: "hello there", createdAt: 1, updatedAt: 2 },
    ]);
    vi.mocked(fetchSessionDetail).mockResolvedValue({
      hasMore: false,
      session: { id: "s1", title: "hello there", createdAt: 1, updatedAt: 2 },
      turns: [
        {
          turn: 1,
          role: "user",
          text: "I are fine.",
          refined: false,
          correction: {
            original: "I are fine.",
            corrected: "I am fine.",
            issues: [
              {
                type: "grammar",
                span: "I are",
                suggestion: "I am",
                explanation: "Subject-verb disagreement.",
                explanationTranslation: "주어-동사 불일치",
              },
            ],
          },
        },
      ],
    });
    const user = userEvent.setup();
    render(<App />);
    await user.click(await screen.findByText("hello there"));
    await screen.findByText("I are fine.");

    await openFeedbackPanel(user);
    expect(await screen.findByText("지금까지 1개 메시지에 피드백이 있어요")).toBeInTheDocument();
    expect(screen.getByText("Subject-verb disagreement.")).toBeInTheDocument();
    expect(screen.getByText("주어-동사 불일치")).toBeInTheDocument();
  });
});

describe("compaction info", () => {
  function openRoomS1() {
    vi.mocked(fetchSessions).mockResolvedValue([
      { id: "s1", title: "hello there", createdAt: 1, updatedAt: 2 },
    ]);
    vi.mocked(fetchSessionDetail).mockResolvedValue({
      hasMore: false,
      session: { id: "s1", title: "hello there", createdAt: 1, updatedAt: 2 },
      turns: [{ turn: 1, role: "user", text: "hi", refined: false }],
    });
  }

  it("does not appear on the room list, nor before a room's id is known", async () => {
    render(<App />);
    await screen.findByRole("button", { name: "Menu" });
    expect(
      screen.queryByRole("button", { name: "대화 압축 상태 보기" }),
    ).not.toBeInTheDocument();

    const user = userEvent.setup();
    await enterNewChat(user); // brand-new room: no id until "ready" arrives
    expect(
      screen.queryByRole("button", { name: "대화 압축 상태 보기" }),
    ).not.toBeInTheDocument();
  });

  it("fetches and shows the summary and turn counts on click", async () => {
    openRoomS1();
    vi.mocked(fetchSessionCompaction).mockResolvedValue({
      summary: "Learner enjoys travel topics; struggles with past perfect tense.",
      recentMessages: 3,
      totalTurns: 20,
    });
    const user = userEvent.setup();
    render(<App />);
    await user.click(await screen.findByText("hello there"));
    await screen.findByText("hi");

    await user.click(screen.getByRole("button", { name: "대화 압축 상태 보기" }));

    expect(fetchSessionCompaction).toHaveBeenCalledWith("s1");
    expect(
      await screen.findByText(/Learner enjoys travel topics/),
    ).toBeInTheDocument();
    expect(screen.getByText(/전체 20턴 중 최근 3개 메시지/)).toBeInTheDocument();
  });

  it("shows a fallback message when the fetch fails", async () => {
    openRoomS1();
    vi.mocked(fetchSessionCompaction).mockResolvedValue(null);
    const user = userEvent.setup();
    render(<App />);
    await user.click(await screen.findByText("hello there"));
    await screen.findByText("hi");

    await user.click(screen.getByRole("button", { name: "대화 압축 상태 보기" }));

    expect(await screen.findByText("불러오지 못했어요.")).toBeInTheDocument();
  });
});

describe("per-message tts playback", () => {
  it("shows a play button for the native rate and each configured extra speed", async () => {
    const user = userEvent.setup();
    render(<App />);
    await enterNewChat(user);
    act(() => emit({ type: "assistant_done", turn: 1, text: "Hello there" }));
    await openStudyPopover(user);
    expect(
      await screen.findByRole("button", { name: "1배속(원어민 속도)으로 재생" }),
    ).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "0.5배속으로 재생" })).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "0.8배속으로 재생" })).toBeInTheDocument();
  });

  it("loads the voice on demand and speaks the message at the chosen rate", async () => {
    const user = userEvent.setup();
    render(<App />);
    await enterNewChat(user);
    act(() => emit({ type: "assistant_done", turn: 1, text: "Hello there" }));
    await openStudyPopover(user);
    await user.click(await screen.findByRole("button", { name: "0.5배속으로 재생" }));
    const speaker = vi.mocked(KokoroSpeaker).mock.instances[0] as unknown as {
      speak: ReturnType<typeof vi.fn>;
    };
    expect(speaker.speak).toHaveBeenCalledWith("Hello there", 0.5);
  });
});

describe("voice enable button", () => {
  it("lets the learner retry from the error pill after a failed load", async () => {
    const user = userEvent.setup();
    render(<App />);
    await enterNewChat(user);
    await openMenu(user);

    const speaker = vi.mocked(KokoroSpeaker).mock.instances[0] as unknown as {
      load: ReturnType<typeof vi.fn>;
    };
    speaker.load.mockRejectedValueOnce(new Error("boom"));

    await user.click(screen.getByRole("button", { name: "음성 활성화" }));
    const retry = await screen.findByRole("button", { name: "음성 다시 불러오기" });
    expect(retry).toBeInTheDocument();

    speaker.load.mockResolvedValueOnce(undefined);
    await user.click(retry);
    expect(await screen.findByText("🔊 음성 준비 완료")).toBeInTheDocument();
  });
});

describe("study popover", () => {
  it("hides the rate buttons until the study button is clicked", async () => {
    const user = userEvent.setup();
    render(<App />);
    await enterNewChat(user);
    act(() => emit({ type: "assistant_done", turn: 1, text: "Hello there" }));
    await screen.findByRole("button", { name: "발음 연습 열기" });
    expect(screen.queryByRole("button", { name: "0.5배속으로 재생" })).not.toBeInTheDocument();
    await openStudyPopover(user);
    expect(screen.getByRole("button", { name: "0.5배속으로 재생" })).toBeInTheDocument();
  });

  it("closes when clicking outside the popover", async () => {
    const user = userEvent.setup();
    render(<App />);
    await enterNewChat(user);
    act(() => emit({ type: "assistant_done", turn: 1, text: "Hello there" }));
    await openStudyPopover(user);
    expect(screen.getByRole("menu")).toBeInTheDocument();
    await user.click(document.body);
    expect(screen.queryByRole("menu")).not.toBeInTheDocument();
  });

  it("closes on Escape", async () => {
    const user = userEvent.setup();
    render(<App />);
    await enterNewChat(user);
    act(() => emit({ type: "assistant_done", turn: 1, text: "Hello there" }));
    await openStudyPopover(user);
    expect(screen.getByRole("menu")).toBeInTheDocument();
    await user.keyboard("{Escape}");
    expect(screen.queryByRole("menu")).not.toBeInTheDocument();
  });
});

describe("typing indicator", () => {
  it("shows immediately when opening a brand-new chat, before the opening greeting arrives", async () => {
    const user = userEvent.setup();
    render(<App />);
    await enterNewChat(user);
    expect(screen.getByRole("status", { name: "답변 생성 중" })).toBeInTheDocument();
  });

  it("disappears once the opening greeting's first token arrives", async () => {
    const user = userEvent.setup();
    render(<App />);
    await enterNewChat(user);
    expect(screen.getByRole("status", { name: "답변 생성 중" })).toBeInTheDocument();

    act(() => emit({ type: "assistant_delta", turn: 0, text: "Hey" }));
    expect(screen.queryByRole("status", { name: "답변 생성 중" })).not.toBeInTheDocument();
  });

  it("shows after sending a typed message and disappears once the reply starts streaming", async () => {
    const user = userEvent.setup();
    render(<App />);
    await enterNewChat(user);
    // Settle the opening greeting first so its own indicator isn't conflated
    // with the one this test is actually checking.
    act(() => emit({ type: "assistant_done", turn: 0, text: "Hey there!" }));
    expect(screen.queryByRole("status", { name: "답변 생성 중" })).not.toBeInTheDocument();

    await user.type(screen.getByPlaceholderText("…or type in English"), "Hello Buddy");
    await user.click(screen.getByRole("button", { name: "Send" }));
    expect(screen.getByRole("status", { name: "답변 생성 중" })).toBeInTheDocument();

    act(() => emit({ type: "final_transcript", turn: 1, text: "Hello Buddy" }));
    expect(screen.getByRole("status", { name: "답변 생성 중" })).toBeInTheDocument();

    act(() => emit({ type: "assistant_delta", turn: 1, text: "Hi" }));
    expect(screen.queryByRole("status", { name: "답변 생성 중" })).not.toBeInTheDocument();
  });

  it("clears if the connection drops while a reply is pending", async () => {
    const user = userEvent.setup();
    render(<App />);
    await enterNewChat(user);
    act(() => emit({ type: "assistant_done", turn: 0, text: "Hey there!" }));

    await user.type(screen.getByPlaceholderText("…or type in English"), "Hello Buddy");
    await user.click(screen.getByRole("button", { name: "Send" }));
    expect(screen.getByRole("status", { name: "답변 생성 중" })).toBeInTheDocument();

    const setStatus = vi.mocked(BuddyClient).mock.calls[
      vi.mocked(BuddyClient).mock.calls.length - 1
    ][1] as (s: string) => void;
    act(() => setStatus("closed"));
    expect(screen.queryByRole("status", { name: "답변 생성 중" })).not.toBeInTheDocument();
  });
});

describe("composer", () => {
  function lastSendText() {
    const mocked = vi.mocked(BuddyClient);
    const instance = mocked.mock.results[mocked.mock.results.length - 1].value as {
      sendText: ReturnType<typeof vi.fn>;
    };
    return instance.sendText;
  }

  it("sends the message and clears the input on Enter", async () => {
    const user = userEvent.setup();
    render(<App />);
    await enterNewChat(user);
    act(() => emit({ type: "assistant_done", turn: 0, text: "Hey there!" }));

    const textarea = screen.getByPlaceholderText("…or type in English");
    await user.type(textarea, "Hello Buddy{Enter}");

    expect(lastSendText()).toHaveBeenCalledWith("Hello Buddy");
    expect(textarea).toHaveValue("");
  });

  it("inserts a newline on Shift+Enter instead of sending", async () => {
    const user = userEvent.setup();
    render(<App />);
    await enterNewChat(user);
    act(() => emit({ type: "assistant_done", turn: 0, text: "Hey there!" }));

    const textarea = screen.getByPlaceholderText("…or type in English");
    await user.type(textarea, "Hello{Shift>}{Enter}{/Shift}Buddy");

    expect(lastSendText()).not.toHaveBeenCalled();
    expect(textarea).toHaveValue("Hello\nBuddy");
  });
});

describe("per-message translations", () => {
  it("shows a small translation line under both the user and assistant bubbles", async () => {
    const user = userEvent.setup();
    render(<App />);
    await enterNewChat(user);
    act(() => emit({ type: "final_transcript", turn: 1, text: "I are fine." }));
    act(() => emit({ type: "user_translation", turn: 1, text: "저는 괜찮아요." }));
    act(() => emit({ type: "assistant_done", turn: 1, text: "Glad to hear it!" }));
    act(() => emit({ type: "assistant_translation", turn: 1, text: "다행이네요!" }));

    expect(await screen.findByText("저는 괜찮아요.")).toBeInTheDocument();
    expect(await screen.findByText("다행이네요!")).toBeInTheDocument();
  });

  it("hydrates persisted translations for both roles when reopening a session", async () => {
    vi.mocked(fetchSessions).mockResolvedValue([
      { id: "s1", title: "hello there", createdAt: 1, updatedAt: 2 },
    ]);
    vi.mocked(fetchSessionDetail).mockResolvedValue({
      hasMore: false,
      session: { id: "s1", title: "hello there", createdAt: 1, updatedAt: 2 },
      turns: [
        { turn: 1, role: "user", text: "hi", refined: false, translation: "안녕" },
        { turn: 1, role: "assistant", text: "hello!", refined: false, translation: "안녕하세요!" },
      ],
    });
    const user = userEvent.setup();
    render(<App />);
    await user.click(await screen.findByText("hello there"));

    expect(await screen.findByText("안녕")).toBeInTheDocument();
    expect(await screen.findByText("안녕하세요!")).toBeInTheDocument();
  });

  it("shows a spinner while a turn's translation is still in flight, live", async () => {
    const user = userEvent.setup();
    render(<App />);
    await enterNewChat(user);
    act(() => emit({ type: "final_transcript", turn: 1, text: "I am fine." }));
    expect(await screen.findAllByRole("status", { name: "번역 중" })).toHaveLength(1);

    act(() => emit({ type: "user_translation", turn: 1, text: "저는 괜찮아요." }));
    expect(await screen.findByText("저는 괜찮아요.")).toBeInTheDocument();
    expect(screen.queryByRole("status", { name: "번역 중" })).not.toBeInTheDocument();

    act(() => emit({ type: "assistant_done", turn: 1, text: "Glad to hear it!" }));
    expect(await screen.findAllByRole("status", { name: "번역 중" })).toHaveLength(1);

    act(() => emit({ type: "assistant_translation", turn: 1, text: "다행이네요!" }));
    expect(await screen.findByText("다행이네요!")).toBeInTheDocument();
    expect(screen.queryByRole("status", { name: "번역 중" })).not.toBeInTheDocument();
  });

  // Guards a real reported bug: leaving a room mid-translation and reopening
  // it used to silently drop the spinner (translation just never showed up),
  // because a hydrated turn with no saved translation rendered nothing. A
  // missing translation on a freshly hydrated turn now shows the same
  // spinner instead, since the server queues it for backfill the moment this
  // fetch lands (see httpserver.sessionDetailHandler).
  it("shows a spinner for a hydrated turn whose translation is still missing", async () => {
    vi.mocked(fetchSessions).mockResolvedValue([
      { id: "s1", title: "hello there", createdAt: 1, updatedAt: 2 },
    ]);
    vi.mocked(fetchSessionDetail).mockResolvedValue({
      hasMore: false,
      session: { id: "s1", title: "hello there", createdAt: 1, updatedAt: 2 },
      turns: [{ turn: 1, role: "user", text: "hi", refined: false }],
    });
    const user = userEvent.setup();
    render(<App />);
    await user.click(await screen.findByText("hello there"));

    await screen.findByText("hi");
    expect(await screen.findByRole("status", { name: "번역 중" })).toBeInTheDocument();
  });

  it("does not show a translation spinner for a hydrated turn that never had one saved and has already stopped polling", async () => {
    vi.mocked(fetchSessions).mockResolvedValue([
      { id: "s1", title: "hello there", createdAt: 1, updatedAt: 2 },
    ]);
    // An empty-text turn (e.g. a stray refined_transcript artifact) never
    // counts as "missing a translation" — there's nothing to translate.
    vi.mocked(fetchSessionDetail).mockResolvedValue({
      hasMore: false,
      session: { id: "s1", title: "hello there", createdAt: 1, updatedAt: 2 },
      turns: [{ turn: 1, role: "user", text: "", refined: false }],
    });
    const user = userEvent.setup();
    render(<App />);
    await user.click(await screen.findByText("hello there"));

    await screen.findByRole("button", { name: "Menu" });
    expect(screen.queryByRole("status", { name: "번역 중" })).not.toBeInTheDocument();
  });
});

// Covers the durable-reply-generation feature (internal/asyncjob): a reply
// still queued/processing when the learner reopens a room (store.Turn.
// ReplyStatus "pending"/"processing") must show the same typing indicator
// as a live in-flight reply, not a blank bubble — and once polling sees it
// finish, the real text must appear and the indicator must clear, the same
// way pollMissingFeedback already recovers a missing translation/correction.
describe("hydrated reply status", () => {
  it("shows the typing indicator, not a blank bubble, for a reply still pending on reopen", async () => {
    vi.mocked(fetchSessions).mockResolvedValue([
      { id: "s1", title: "hello there", createdAt: 1, updatedAt: Date.now() / 1000 },
    ]);
    vi.mocked(fetchSessionDetail).mockResolvedValue({
      hasMore: false,
      session: { id: "s1", title: "hello there", createdAt: 1, updatedAt: Date.now() / 1000 },
      turns: [
        { turn: 1, role: "user", text: "hi", refined: false },
        { turn: 1, role: "assistant", text: "", refined: false, replyStatus: "pending" },
      ],
    });
    const user = userEvent.setup();
    render(<App />);
    await user.click(await screen.findByText("hello there"));

    await screen.findByText("hi");
    expect(screen.getByRole("status", { name: "답변 생성 중" })).toBeInTheDocument();
    // The pending placeholder itself must not render as an empty bubble —
    // only the "hi" user turn and the typing indicator should be present.
    expect(screen.getAllByText("hi")).toHaveLength(1);
  });

  it("materializes the reply and clears the typing indicator once polling sees it finish", async () => {
    // Fake timers from the start, so pollMissingFeedback's setTimeout(…,
    // 4000) is one we control (see the advanceTimersByTimeAsync(4000) below)
    // instead of a real one we'd have to wait out. The room click below uses
    // fireEvent rather than userEvent — userEvent's click hangs forever once
    // setTimeout is faked (it awaits something that never settles without
    // real time passing), even with delay: null/advanceTimers configured;
    // fireEvent dispatches the event directly through a single act() and
    // isn't affected. { shouldAdvanceTime: true } used to paper over this by
    // ticking the mocked clock off the real wall clock instead, but that
    // raced findByText's own timeout under CI load and flaked; this version
    // never depends on real elapsed time.
    vi.useFakeTimers();
    try {
      vi.mocked(fetchSessions).mockResolvedValue([
        { id: "s1", title: "hello there", createdAt: 1, updatedAt: Date.now() / 1000 },
      ]);
      vi.mocked(fetchSessionDetail).mockResolvedValueOnce({
        hasMore: false,
        session: { id: "s1", title: "hello there", createdAt: 1, updatedAt: Date.now() / 1000 },
        turns: [
          { turn: 1, role: "user", text: "hi", refined: false },
          { turn: 1, role: "assistant", text: "", refined: false, replyStatus: "pending" },
        ],
      });
      render(<App />);
      await flushRenders();
      fireEvent.click(screen.getByText("hello there"));
      await flushRenders();
      expect(screen.getByText("hi")).toBeInTheDocument();
      expect(screen.getByRole("status", { name: "답변 생성 중" })).toBeInTheDocument();

      vi.mocked(fetchSessionDetail).mockResolvedValue({
        hasMore: false,
        session: { id: "s1", title: "hello there", createdAt: 1, updatedAt: Date.now() / 1000 },
        turns: [
          { turn: 1, role: "user", text: "hi", refined: false },
          { turn: 1, role: "assistant", text: "recovered on another replica", refined: false, replyStatus: "done" },
        ],
      });
      await vi.advanceTimersByTimeAsync(4000);
      await flushRenders();

      expect(screen.getByText("recovered on another replica")).toBeInTheDocument();
      expect(screen.queryByRole("status", { name: "답변 생성 중" })).not.toBeInTheDocument();
    } finally {
      vi.useRealTimers();
    }
  });
});

describe("date dividers and message times", () => {
  // Only Date is faked, to pin what "now" is for formatDateDivider's
  // 오늘/어제 logic — setTimeout/setInterval stay real, since faking them
  // wholesale makes userEvent.click hang forever (its wait never settles
  // without real time passing, see "materializes the reply…" above) and
  // needs every resulting render flushed by hand. { shouldAdvanceTime: true
  // } used to be needed here to keep those real-timer-dependent bits alive
  // under full fake timers, but it ties the clock's advancement to the real
  // wall clock and flakes under CI load; toFake: ["Date"] sidesteps the
  // whole problem by never faking setTimeout in the first place.
  afterEach(() => {
    vi.useRealTimers();
  });

  it("shows one divider per calendar day and a time under every message when hydrating a session", async () => {
    vi.useFakeTimers({ toFake: ["Date"] });
    vi.setSystemTime(new Date(2024, 5, 15, 10, 0, 0));

    const yesterdayUser = new Date(2024, 5, 14, 9, 5, 0).getTime() / 1000;
    const yesterdayAssistant = new Date(2024, 5, 14, 9, 6, 0).getTime() / 1000;
    const todayUser = new Date(2024, 5, 15, 9, 7, 0).getTime() / 1000;

    vi.mocked(fetchSessions).mockResolvedValue([
      { id: "s1", title: "hello there", createdAt: 1, updatedAt: 2 },
    ]);
    vi.mocked(fetchSessionDetail).mockResolvedValue({
      hasMore: false,
      session: { id: "s1", title: "hello there", createdAt: 1, updatedAt: 2 },
      turns: [
        { turn: 1, role: "user", text: "hi", refined: false, createdAt: yesterdayUser },
        { turn: 1, role: "assistant", text: "hello!", refined: false, createdAt: yesterdayAssistant },
        { turn: 2, role: "user", text: "bye", refined: false, createdAt: todayUser },
      ],
    });
    const user = userEvent.setup();
    render(<App />);
    await user.click(await screen.findByText("hello there"));

    expect(await screen.findByText("어제")).toBeInTheDocument();
    expect(await screen.findByText("오늘")).toBeInTheDocument();
    expect(screen.getByText("오전 9:05")).toBeInTheDocument();
    expect(screen.getByText("오전 9:06")).toBeInTheDocument();
    expect(screen.getByText("오전 9:07")).toBeInTheDocument();
  });

  it("stamps a live message with today's divider and the current clock time", async () => {
    vi.useFakeTimers({ toFake: ["Date"] });
    vi.setSystemTime(new Date(2024, 5, 15, 14, 30, 0));

    const user = userEvent.setup();
    render(<App />);
    await enterNewChat(user);
    act(() => emit({ type: "final_transcript", turn: 1, text: "Hello" }));

    expect(await screen.findByText("오늘")).toBeInTheDocument();
    expect(await screen.findByText("오후 2:30")).toBeInTheDocument();
  });
});

describe("tts speed settings", () => {
  it("lists the native speed as fixed and the default extra speeds as removable", async () => {
    const user = userEvent.setup();
    render(<App />);
    await enterNewChat(user);
    await openMenu(user);
    expect(screen.getByText("🔊 1x (원어민)")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "0.5x 속도 삭제" })).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "0.8x 속도 삭제" })).toBeInTheDocument();
  });

  it("hides the add-speed form once the two extra-speed slots are full", async () => {
    const user = userEvent.setup();
    render(<App />);
    await enterNewChat(user);
    await openMenu(user);
    expect(screen.queryByLabelText("새 재생 속도")).not.toBeInTheDocument();
  });

  it("removing a speed frees a slot, drops its play button, and persists the change", async () => {
    const user = userEvent.setup();
    render(<App />);
    await enterNewChat(user);
    act(() => emit({ type: "assistant_done", turn: 1, text: "Hello there" }));
    await openMenu(user);
    await user.click(screen.getByRole("button", { name: "0.5x 속도 삭제" }));
    expect(screen.getByLabelText("새 재생 속도")).toBeInTheDocument();
    expect(localStorage.getItem("buddy.tts.extraRates")).toBe(JSON.stringify([0.8]));
    // Opening the study popover closes the menu (click lands outside it),
    // which is fine — the assertions above already ran against the menu.
    await openStudyPopover(user);
    expect(screen.queryByRole("button", { name: "0.5배속으로 재생" })).not.toBeInTheDocument();
  });

  it("adds a custom speed once a slot is free and persists it", async () => {
    const user = userEvent.setup();
    render(<App />);
    await enterNewChat(user);
    await openMenu(user);
    await user.click(screen.getByRole("button", { name: "0.5x 속도 삭제" }));
    await user.type(screen.getByLabelText("새 재생 속도"), "1.5");
    await user.click(screen.getByRole("button", { name: "추가" }));
    expect(screen.getByRole("button", { name: "1.5x 속도 삭제" })).toBeInTheDocument();
    expect(localStorage.getItem("buddy.tts.extraRates")).toBe(JSON.stringify([0.8, 1.5]));
    expect(screen.queryByLabelText("새 재생 속도")).not.toBeInTheDocument();
  });

  it("rejects the native rate as a custom speed", async () => {
    const user = userEvent.setup();
    render(<App />);
    await enterNewChat(user);
    await openMenu(user);
    await user.click(screen.getByRole("button", { name: "0.5x 속도 삭제" }));
    await user.type(screen.getByLabelText("새 재생 속도"), "1");
    await user.click(screen.getByRole("button", { name: "추가" }));
    expect(localStorage.getItem("buddy.tts.extraRates")).toBe(JSON.stringify([0.8]));
  });

  it("persists the default speeds to localStorage on first load", () => {
    render(<App />);
    expect(localStorage.getItem("buddy.tts.extraRates")).toBe(JSON.stringify([0.5, 0.8]));
  });
});
