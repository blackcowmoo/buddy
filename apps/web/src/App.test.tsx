/**
 * @vitest-environment jsdom
 */
import "@testing-library/jest-dom/vitest";
import { act, cleanup, render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import type { ServerEvent } from "./lib/protocol";

let capturedOnEvent: ((e: ServerEvent) => void) | null = null;

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
  deleteSession: vi.fn(),
}));

import { App } from "./App";
import { fetchMe } from "./lib/me";
import { deleteSession, fetchSessionDetail, fetchSessions } from "./lib/sessions";
import { KokoroSpeaker } from "./tts/kokoro";
import { BuddyClient } from "./lib/ws";

beforeEach(() => {
  localStorage.clear();
  capturedOnEvent = null;
  vi.mocked(fetchMe).mockResolvedValue(null);
  vi.mocked(fetchSessions).mockResolvedValue([]);
  vi.mocked(fetchSessionDetail).mockResolvedValue(null);
  vi.stubGlobal("location", {
    protocol: "http:",
    host: "buddy.example",
    pathname: "/",
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
    expect(fetchSessionDetail).toHaveBeenCalledWith("s1");
    expect(lastClientInstance().connect).toHaveBeenCalledWith("s1");
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

  it('"back to list" closes the connection and re-shows the room list', async () => {
    const user = userEvent.setup();
    render(<App />);
    await enterNewChat(user);

    await user.click(screen.getByRole("button", { name: "목록으로" }));

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
    expect(screen.queryByText("Enable voice")).not.toBeInTheDocument();
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
              explanation: "앞 문맥과 맞지 않아요",
            },
          ],
        },
      }),
    );
    await openGrammarPopover(user);
    expect(await screen.findByText("문맥")).toBeInTheDocument();
    expect(screen.getByText("앞 문맥과 맞지 않아요")).toBeInTheDocument();
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

  it("does not show a spinner for a hydrated turn with no saved correction", async () => {
    vi.mocked(fetchSessions).mockResolvedValue([
      { id: "s1", title: "hello there", createdAt: 1, updatedAt: 2 },
    ]);
    vi.mocked(fetchSessionDetail).mockResolvedValue({
      session: { id: "s1", title: "hello there", createdAt: 1, updatedAt: 2 },
      turns: [{ turn: 1, role: "user", text: "hi", refined: false }],
    });
    const user = userEvent.setup();
    render(<App />);
    await user.click(await screen.findByText("hello there"));

    await screen.findByText("hi");
    expect(screen.queryByRole("button", { name: /^문법/ })).not.toBeInTheDocument();
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
