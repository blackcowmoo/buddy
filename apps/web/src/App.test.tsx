/**
 * @vitest-environment jsdom
 */
import "@testing-library/jest-dom/vitest";
import { cleanup, render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

vi.mock("./lib/ws", () => ({
  BuddyClient: vi.fn().mockImplementation(function BuddyClient(this: object) {
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
import { BuddyClient } from "./lib/ws";

beforeEach(() => {
  vi.mocked(fetchMe).mockResolvedValue(null);
  vi.mocked(fetchSessions).mockResolvedValue([]);
  vi.mocked(fetchSessionDetail).mockResolvedValue(null);
  vi.stubGlobal("location", {
    protocol: "http:",
    host: "buddy.example",
    pathname: "/",
    assign: vi.fn(),
  });
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  vi.clearAllMocks();
});

function openMenu(user: ReturnType<typeof userEvent.setup>) {
  return user.click(screen.getByRole("button", { name: "Menu" }));
}

// The app always lands on the room list; the hamburger menu only exists in
// chat view, so every menu test needs to get there first, the same way a
// learner would: start a new chat.
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
