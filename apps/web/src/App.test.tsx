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
      reset: vi.fn(),
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
}));

import { App } from "./App";
import { fetchMe } from "./lib/me";
import { fetchSessionDetail, fetchSessions } from "./lib/sessions";
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

  it("closes the menu when resetting the conversation", async () => {
    const user = userEvent.setup();
    render(<App />);
    await enterNewChat(user);
    await openMenu(user);
    await user.click(screen.getByRole("menuitem", { name: /대화 초기화/ }));
    expect(screen.queryByRole("menu")).not.toBeInTheDocument();
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

describe("per-message tts playback", () => {
  it("shows a play button for the native rate and each configured extra speed", async () => {
    const user = userEvent.setup();
    render(<App />);
    await enterNewChat(user);
    act(() => emit({ type: "assistant_done", turn: 1, text: "Hello there" }));
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
    await user.click(await screen.findByRole("button", { name: "0.5배속으로 재생" }));
    const speaker = vi.mocked(KokoroSpeaker).mock.instances[0] as unknown as {
      speak: ReturnType<typeof vi.fn>;
    };
    expect(speaker.speak).toHaveBeenCalledWith("Hello there", 0.5);
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
    expect(screen.queryByRole("button", { name: "0.5배속으로 재생" })).not.toBeInTheDocument();
    expect(screen.getByLabelText("새 재생 속도")).toBeInTheDocument();
    expect(localStorage.getItem("buddy.tts.extraRates")).toBe(JSON.stringify([0.8]));
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
