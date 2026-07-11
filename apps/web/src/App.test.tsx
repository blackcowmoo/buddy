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

import { App } from "./App";
import { fetchMe } from "./lib/me";

beforeEach(() => {
  vi.mocked(fetchMe).mockResolvedValue(null);
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

describe("hamburger menu", () => {
  it("is closed by default", () => {
    render(<App />);
    expect(screen.queryByRole("menu")).not.toBeInTheDocument();
  });

  it("opens on click and closes on a second click", async () => {
    const user = userEvent.setup();
    render(<App />);
    await openMenu(user);
    expect(screen.getByRole("menu")).toBeInTheDocument();
    await openMenu(user);
    expect(screen.queryByRole("menu")).not.toBeInTheDocument();
  });

  it("closes when clicking outside the menu", async () => {
    const user = userEvent.setup();
    render(<App />);
    await openMenu(user);
    expect(screen.getByRole("menu")).toBeInTheDocument();
    await user.click(document.body);
    expect(screen.queryByRole("menu")).not.toBeInTheDocument();
  });

  it("closes on Escape", async () => {
    const user = userEvent.setup();
    render(<App />);
    await openMenu(user);
    expect(screen.getByRole("menu")).toBeInTheDocument();
    await user.keyboard("{Escape}");
    expect(screen.queryByRole("menu")).not.toBeInTheDocument();
  });

  it("shows the oidc email once fetched", async () => {
    vi.mocked(fetchMe).mockResolvedValue({ identityMode: "oidc", id: "alex@example.com" });
    const user = userEvent.setup();
    render(<App />);
    await openMenu(user);
    expect(await screen.findByText("alex@example.com")).toBeInTheDocument();
  });

  it("shows an anonymous label when there is no real identity", async () => {
    const user = userEvent.setup();
    render(<App />);
    await openMenu(user);
    expect(await screen.findByText("익명 사용자")).toBeInTheDocument();
  });

  it("rejects a non-numeric PR path and does not navigate", async () => {
    const user = userEvent.setup();
    render(<App />);
    await openMenu(user);
    await user.type(screen.getByLabelText("PR 미리보기로 이동"), "abc");
    await user.click(screen.getByRole("button", { name: "이동" }));
    expect(await screen.findByText(/숫자만 입력하세요/)).toBeInTheDocument();
    expect(location.assign).not.toHaveBeenCalled();
  });

  it("navigates to /pr/<n>/ for a numeric PR input", async () => {
    const user = userEvent.setup();
    render(<App />);
    await openMenu(user);
    await user.type(screen.getByLabelText("PR 미리보기로 이동"), "14");
    await user.click(screen.getByRole("button", { name: "이동" }));
    expect(location.assign).toHaveBeenCalledWith("/pr/14/");
  });

  it("closes the menu when resetting the conversation", async () => {
    const user = userEvent.setup();
    render(<App />);
    await openMenu(user);
    await user.click(screen.getByRole("menuitem", { name: /대화 초기화/ }));
    expect(screen.queryByRole("menu")).not.toBeInTheDocument();
  });
});
