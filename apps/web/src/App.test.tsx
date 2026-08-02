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
  endSession: vi.fn(),
  restudySession: vi.fn(),
  markQuizCompleted: vi.fn(),
  resetQuiz: vi.fn(),
}));

vi.mock("./lib/settings", () => ({
  fetchSettings: vi.fn(),
  saveSettings: vi.fn(),
  MAX_INTERLOCUTOR_STYLE_LEN: 1024,
}));

vi.mock("./lib/wordSearch", () => ({
  suggestWords: vi.fn(),
}));

vi.mock("./lib/wordReview", () => ({
  saveWord: vi.fn(),
  fetchWords: vi.fn(),
}));

vi.mock("./lib/quizCheck", async (importOriginal) => ({
  ...(await importOriginal<typeof import("./lib/quizCheck")>()),
  checkQuizAnswer: vi.fn(),
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
  endSession,
  fetchSessionCompaction,
  fetchSessionDetail,
  fetchSessions,
  markQuizCompleted,
  resetQuiz,
  restudySession,
} from "./lib/sessions";
import { fetchSettings, saveSettings } from "./lib/settings";
import { suggestWords } from "./lib/wordSearch";
import { saveWord, fetchWords } from "./lib/wordReview";
import { checkQuizAnswer } from "./lib/quizCheck";
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
  vi.mocked(restudySession).mockResolvedValue(true);
  vi.mocked(markQuizCompleted).mockResolvedValue(true);
  vi.mocked(resetQuiz).mockResolvedValue(true);
  // Default to "not confirmed correct" — matches quizAnswerCheckSystemPrompt's
  // own bias server-side, so a test that doesn't care about the LLM fallback
  // path still exercises the same "wrong unless proven otherwise" behavior.
  vi.mocked(checkQuizAnswer).mockResolvedValue(false);
  vi.mocked(fetchSettings).mockResolvedValue({ interlocutorStyle: "", learnerProfile: "" });
  vi.mocked(saveSettings).mockResolvedValue(true);
  vi.mocked(suggestWords).mockResolvedValue([]);
  vi.mocked(saveWord).mockResolvedValue(null);
  vi.mocked(fetchWords).mockResolvedValue({ words: [], dueCount: 0 });
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
  // cleanup() first, while any fake timers a test installed are still in
  // place: unmounting is what cancels App's in-flight polls (see "stops
  // polling once the app unmounts"), and it has to clear the same timer
  // implementation that scheduled them.
  cleanup();
  vi.useRealTimers();
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
// due "now", but only drains one microtask "layer" per call, so a render
// that chains multiple .then()s needs it called more than once. Note that
// "the DOM didn't change this tick" is NOT a valid stopping condition either
// (tried that; it flaked immediately) — nothing may have happened yet on an
// early tick, which looks identical to "already settled" from the outside. A
// fixed iteration count used to paper over this by guessing how many ticks
// the current code path happens to need, which is a property of the promise
// chain's shape and the test environment's scheduling, not of the code being
// correct — it flaked in CI once that guess (5) undercounted. The only
// actually-correct stopping condition is the real one the caller is waiting
// for, so this advances one tick at a time until check() says so, capped
// well above anything real as a safety net against a genuinely hung update
// rather than a substitute for checking the real condition.
//
// Every advance of vitest's fake timers in this file — this tick included —
// must be wrapped in act(), same as the act(() => emit(...)) calls below for
// WS events. Without it, React is free to leave the resulting state update
// queued rather than flushed by the time check() runs right after, so the
// loop can still observe "not yet" on every tick despite the promise chain
// having actually settled — indistinguishable from a real hang from the
// outside, except it only shows up under CI's scheduling, never locally.
// This function already flaked in CI three times chasing this: once as a
// fixed-iteration-count guess, once as this tick loop without act() here,
// and once more after that fix landed — because the very next line in the
// test using this helper (the standalone `vi.advanceTimersByTimeAsync(4000)`
// past the pollMissingFeedback retry) had the same gap and wasn't caught
// along with this one. Every fake-timer advance anywhere in this file needs
// the same treatment — this loop alone isn't sufficient if a call site
// upstream of it skips act().
async function flushUntil(check: () => boolean, maxTicks = 50) {
  for (let i = 0; i < maxTicks; i++) {
    if (check()) return;
    await act(() => vi.advanceTimersByTimeAsync(0));
  }
  if (!check()) {
    throw new Error(`flushUntil: condition still false after ${maxTicks} ticks`);
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

  // Guards the "still in progress" indicator this feature adds: a room
  // whose background study-summary job hasn't finished yet (see
  // asyncjob.KindStudySummary) must show that in the list itself, not just
  // once the room is reopened — the whole point is that leaving and coming
  // back should let the learner tell whether it's still working.
  it("shows a pending badge for an ended session whose study summary is still generating", async () => {
    vi.mocked(fetchSessions).mockResolvedValue([
      { id: "s1", title: "hello there", createdAt: 1, updatedAt: 2, ended: true, studySummaryStatus: "pending" },
      { id: "s2", title: "already done", createdAt: 1, updatedAt: 2, ended: true, studySummaryStatus: "done" },
    ]);
    render(<App />);
    expect(await screen.findByText("정리 중")).toBeInTheDocument();
    expect(screen.queryByText("정리 중")?.closest("li")?.textContent).toContain("hello there");
    expect(screen.getByText("already done").closest("li")?.textContent).not.toContain("정리 중");
  });

  // Guards the "all correct" checkmark this feature adds to the room list
  // (see store.SessionMeta.QuizCompleted/markQuizCompleted): only a session
  // with quizCompleted true shows it, distinguishing it from an ended room
  // that hasn't (yet) studied its quiz.
  it("shows a completed badge only for a session whose quiz was marked completed", async () => {
    vi.mocked(fetchSessions).mockResolvedValue([
      { id: "s1", title: "hello there", createdAt: 1, updatedAt: 2, ended: true, studySummaryStatus: "done", quizCompleted: true },
      { id: "s2", title: "not yet studied", createdAt: 1, updatedAt: 2, ended: true, studySummaryStatus: "done", quizCompleted: false },
    ]);
    render(<App />);
    expect(await screen.findByText("✅ 학습 완료")).toBeInTheDocument();
    expect(screen.queryByText("✅ 학습 완료")?.closest("li")?.textContent).toContain("hello there");
    expect(screen.getByText("not yet studied").closest("li")?.textContent).not.toContain("학습 완료");
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

  // Guards the permanent-freeze half of the end-conversation feature
  // (see EndConversationControl/endSession in App.tsx): a room whose
  // fetched detail reports it as already `ended` must render read-only —
  // no live connection kept open, composer removed (not just disabled) —
  // even though the WS handshake is fired eagerly (in parallel with the
  // fetch) for every room.
  it("reopening an ended session closes the live connection and removes the composer", async () => {
    vi.mocked(fetchSessions).mockResolvedValue([
      { id: "s1", title: "hello there", createdAt: 1, updatedAt: 2, ended: true },
    ]);
    vi.mocked(fetchSessionDetail).mockResolvedValue({
      hasMore: false,
      session: {
        id: "s1",
        title: "hello there",
        createdAt: 1,
        updatedAt: 2,
        ended: true,
        studySummary: [{ english: "Focus on third-person -s.", translation: "3인칭 단수 -s에 집중하세요." }],
      },
      turns: [{ turn: 1, role: "user", text: "hi", refined: false }],
    });
    const user = userEvent.setup();
    render(<App />);
    await user.click(await screen.findByText("hello there"));

    expect(await screen.findByText("hi")).toBeInTheDocument();
    expect(lastClientInstance().connect).toHaveBeenCalledWith("s1"); // fired eagerly...
    expect(lastClientInstance().close).toHaveBeenCalled(); // ...then dropped once `ended` is known

    expect(screen.queryByRole("button", { name: "Send" })).not.toBeInTheDocument();
    expect(screen.queryByRole("textbox")).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Push to talk" })).not.toBeInTheDocument();
    expect(screen.getByText("이 대화는 종료되어 더 이상 메시지를 보낼 수 없어요.")).toBeInTheDocument();

    // A stray event arriving anyway (e.g. a slow in-flight job from before
    // the close) must not resurrect the room as if it were live.
    await act(async () => emit({ type: "assistant_delta", turn: 2, text: "should not appear" }));
    expect(screen.queryByText("should not appear")).not.toBeInTheDocument();

    // Merely viewing an already-ended room must never re-trigger the
    // confirm-and-end flow on its own.
    expect(endSession).not.toHaveBeenCalled();
  });

  // Opening the panel must ask before ending: the freeze/background wrap-up
  // job should only kick off once the learner explicitly confirms, not on
  // every tap/peek of the 🎓 icon.
  it("opening the end-conversation panel asks first and does not end the session until confirmed", async () => {
    vi.mocked(fetchSessions).mockResolvedValue([
      { id: "s1", title: "hello there", createdAt: 1, updatedAt: 2, ended: false },
    ]);
    vi.mocked(fetchSessionDetail).mockResolvedValue({
      hasMore: false,
      session: { id: "s1", title: "hello there", createdAt: 1, updatedAt: 2, ended: false },
      turns: [{ turn: 1, role: "user", text: "hi", refined: false }],
    });
    const user = userEvent.setup();
    render(<App />);
    await user.click(await screen.findByText("hello there"));
    expect(await screen.findByText("hi")).toBeInTheDocument();

    await user.click(screen.getByRole("button", { name: "대화 종료" }));
    expect(
      await screen.findByText(
        "대화를 종료할까요? 종료하면 지금까지의 대화를 바탕으로 학습 피드백을 정리해요.",
      ),
    ).toBeInTheDocument();
    expect(endSession).not.toHaveBeenCalled();
  });

  // Guards the core fix this feature is about: confirming "end this
  // conversation" freezes the room and returns to the list immediately —
  // it must not wait on the study-summary wrap-up (that's a background job
  // from here on, see EnqueueStudySummaryJob/RunStudySummaryInline
  // server-side), so nothing here is lost if the learner navigates away
  // right after confirming.
  it("confirming end conversation freezes the session and returns to the list immediately", async () => {
    vi.mocked(fetchSessions).mockResolvedValue([
      { id: "s1", title: "hello there", createdAt: 1, updatedAt: 2, ended: false },
    ]);
    vi.mocked(fetchSessionDetail).mockResolvedValue({
      hasMore: false,
      session: { id: "s1", title: "hello there", createdAt: 1, updatedAt: 2, ended: false },
      turns: [{ turn: 1, role: "user", text: "hi", refined: false }],
    });
    const user = userEvent.setup();
    render(<App />);
    await user.click(await screen.findByText("hello there"));
    expect(await screen.findByText("hi")).toBeInTheDocument();

    await user.click(screen.getByRole("button", { name: "대화 종료" }));
    await user.click(screen.getByRole("button", { name: "예, 종료할래요" }));

    expect(endSession).toHaveBeenCalledWith("s1");
    // Leaving reuses the room's history entry (see goBack), same as the
    // plain back button — a real browser resolves that asynchronously via
    // popstate, which App's mount effect listens for.
    expect(goBack).toHaveBeenCalled();
    act(() => capturedPopStateHandler?.({ view: "list" }));

    expect(await screen.findByRole("button", { name: "+ 새 대화" })).toBeInTheDocument();
    expect(lastClientInstance().close).toHaveBeenCalled();
  });

  // Guards the "still generating" indicator: reopening a room that's
  // ended but whose background wrap-up job hasn't finished yet must show a
  // loading state, not a blank/stuck panel — and must pick up the finished
  // summary once a later poll (see pollStudySummary) sees it land, without
  // needing to leave and reopen the room.
  it("reopening a room whose study summary is still pending shows a loading state, then the summary once it lands", async () => {
    vi.useFakeTimers();
    try {
      vi.mocked(fetchSessions).mockResolvedValue([
        { id: "s1", title: "hello there", createdAt: 1, updatedAt: 2, ended: true, studySummaryStatus: "pending" },
      ]);
      vi.mocked(fetchSessionDetail).mockResolvedValueOnce({
        hasMore: false,
        session: { id: "s1", title: "hello there", createdAt: 1, updatedAt: 2, ended: true, studySummaryStatus: "pending" },
        turns: [{ turn: 1, role: "user", text: "hi", refined: false }],
      });
      render(<App />);
      await flushUntil(() => screen.queryByText("hello there") !== null);
      fireEvent.click(screen.getByText("hello there"));
      await flushUntil(() => screen.queryByText("hi") !== null);

      fireEvent.click(screen.getByRole("button", { name: "대화 종료" }));
      await flushUntil(() => screen.queryByText("학습 피드백을 정리하는 중…") !== null);

      vi.mocked(fetchSessionDetail).mockResolvedValue({
        hasMore: false,
        session: {
          id: "s1",
          title: "hello there",
          createdAt: 1,
          updatedAt: 2,
          ended: true,
          studySummary: [{ english: "Focus on third-person -s.", translation: "3인칭 단수 -s에 집중하세요." }],
          studySummaryStatus: "done",
        },
        turns: [{ turn: 1, role: "user", text: "hi", refined: false }],
      });
      await act(() => vi.advanceTimersByTimeAsync(4000));
      await flushUntil(() => screen.queryByText("Focus on third-person -s.") !== null);
      // The Korean translation must render alongside the English sentence,
      // not just the English half of the pair.
      expect(screen.getByText("3인칭 단수 -s에 집중하세요.")).toBeInTheDocument();
    } finally {
      vi.useRealTimers();
    }
  });

  // Mirrors the study-summary polling test above, but for the quiz
  // pre-generation job running independently alongside it (see
  // pollQuizStatus/asyncjob.KindStudyQuiz): a room reopened while the quiz
  // job is still pending shows the "준비하는 중" hint, not a dead "퀴즈 풀기"
  // button, and picks up the "퀴즈 풀기" button once a later poll sees it land
  // — all while the study summary itself was already done from the start.
  it("reopening a room whose quiz is still pending shows a preparing hint, then the quiz button once it lands", async () => {
    vi.useFakeTimers();
    try {
      vi.mocked(fetchSessions).mockResolvedValue([
        { id: "s1", title: "hello there", createdAt: 1, updatedAt: 2, ended: true, studySummaryStatus: "done", quizStatus: "pending" },
      ]);
      vi.mocked(fetchSessionDetail).mockResolvedValueOnce({
        hasMore: false,
        session: {
          id: "s1",
          title: "hello there",
          createdAt: 1,
          updatedAt: 2,
          ended: true,
          studySummary: [{ english: "Focus on third-person -s.", translation: "3인칭 단수 -s에 집중하세요." }],
          studySummaryStatus: "done",
          quizStatus: "pending",
        },
        turns: [{ turn: 1, role: "user", text: "hi", refined: false }],
      });
      render(<App />);
      await flushUntil(() => screen.queryByText("hello there") !== null);
      fireEvent.click(screen.getByText("hello there"));
      await flushUntil(() => screen.queryByText("hi") !== null);

      fireEvent.click(screen.getByRole("button", { name: "대화 종료" }));
      await flushUntil(() => screen.queryByText("퀴즈를 준비하는 중…") !== null);
      expect(screen.queryByText("퀴즈 풀기")).not.toBeInTheDocument();

      vi.mocked(fetchSessionDetail).mockResolvedValue({
        hasMore: false,
        session: {
          id: "s1",
          title: "hello there",
          createdAt: 1,
          updatedAt: 2,
          ended: true,
          studySummary: [{ english: "Focus on third-person -s.", translation: "3인칭 단수 -s에 집중하세요." }],
          studySummaryStatus: "done",
          quiz: [
            {
              prompt: "He ___ to school.",
              answer: "goes",
              translation: "그는 학교에 가요.",
              explanation: "unused",
              explanationTranslation: "unused",
            },
          ],
          quizStatus: "done",
        },
        turns: [{ turn: 1, role: "user", text: "hi", refined: false }],
      });
      await act(() => vi.advanceTimersByTimeAsync(4000));
      await flushUntil(() => screen.queryByText("퀴즈 풀기") !== null);
    } finally {
      vi.useRealTimers();
    }
  });

  // Guards the pre-generated quiz flow (see EndConversationControl/
  // QuizPanel): the questions already arrive on session.quiz (pre-generated
  // alongside the wrap-up — see asyncjob.KindStudyQuiz), so "퀴즈 풀기" just
  // opens the panel with no fetch of its own. Grades a wrong answer as wrong
  // while still surfacing the correct one, grades a matching answer as right
  // even with different casing/whitespace (see normalizeQuizAnswer), advances
  // across questions, lands on a final score, and — since one answer here is
  // wrong — must never mark the quiz completed (see markQuizCompleted).
  it("opens the pre-generated quiz from the study feedback panel, grades answers, and shows a final score", async () => {
    vi.mocked(fetchSessions).mockResolvedValue([
      { id: "s1", title: "hello there", createdAt: 1, updatedAt: 2, ended: true, studySummaryStatus: "done" },
    ]);
    vi.mocked(fetchSessionDetail).mockResolvedValue({
      hasMore: false,
      session: {
        id: "s1",
        title: "hello there",
        createdAt: 1,
        updatedAt: 2,
        ended: true,
        studySummary: [{ english: "Focus on third-person -s.", translation: "3인칭 단수 -s에 집중하세요." }],
        studySummaryStatus: "done",
        quiz: [
          {
            prompt: "He ___ to school every day.",
            answer: "goes",
            translation: "그는 매일 학교에 가요.",
            explanation: "Third person singular needs -s.",
            explanationTranslation: "3인칭 단수는 -s가 필요해요.",
          },
          {
            prompt: "She likes ___ books.",
            answer: "reading",
            translation: "그녀는 책 읽는 것을 좋아해요.",
            explanation: "The verb after \"likes\" takes the -ing form here.",
            explanationTranslation: "\"likes\" 다음에는 -ing 형태가 와요.",
          },
        ],
        quizStatus: "done",
      },
      turns: [{ turn: 1, role: "user", text: "hi", refined: false }],
    });
    const user = userEvent.setup();
    render(<App />);
    await user.click(await screen.findByText("hello there"));
    await screen.findByText("hi");

    await user.click(screen.getByRole("button", { name: "대화 종료" }));
    await user.click(await screen.findByText("퀴즈 풀기"));

    // Question 1: a wrong answer still names the correct one.
    expect(await screen.findByText("He ___ to school every day.")).toBeInTheDocument();
    await user.type(screen.getByRole("textbox", { name: "정답 입력" }), "go");
    await user.click(screen.getByRole("button", { name: "확인" }));
    expect(await screen.findByText("아쉬워요. 정답: goes")).toBeInTheDocument();
    expect(screen.getByText("Third person singular needs -s.")).toBeInTheDocument();

    await user.click(screen.getByRole("button", { name: "다음 문제" }));

    // Question 2: matches despite different casing/trailing punctuation/whitespace.
    expect(await screen.findByText("She likes ___ books.")).toBeInTheDocument();
    await user.type(screen.getByRole("textbox", { name: "정답 입력" }), "  READING. ");
    await user.click(screen.getByRole("button", { name: "확인" }));
    expect(await screen.findByText("정답이에요!")).toBeInTheDocument();

    expect(screen.getByText("2문제 중 1개 맞혔어요!")).toBeInTheDocument();
    expect(markQuizCompleted).not.toHaveBeenCalled();
  });

  // Guards against the popover's own outside-click dismissal (see useDismiss)
  // discarding quiz progress: an accidental click outside the panel while
  // mid-quiz used to reset both quizMode and QuizPanel's own state, so
  // reopening always restarted from question 1. Progress now lives in
  // EndConversationControl (see quizIndex/quizAnswer/etc.), which survives
  // the close, so reopening must land back on the same question.
  it("keeps quiz progress after an outside click closes the popover and it's reopened", async () => {
    vi.mocked(fetchSessions).mockResolvedValue([
      { id: "s1", title: "hello there", createdAt: 1, updatedAt: 2, ended: true, studySummaryStatus: "done" },
    ]);
    vi.mocked(fetchSessionDetail).mockResolvedValue({
      hasMore: false,
      session: {
        id: "s1",
        title: "hello there",
        createdAt: 1,
        updatedAt: 2,
        ended: true,
        studySummary: [{ english: "Focus on third-person -s.", translation: "3인칭 단수 -s에 집중하세요." }],
        studySummaryStatus: "done",
        quiz: [
          {
            prompt: "He ___ to school every day.",
            answer: "goes",
            translation: "그는 매일 학교에 가요.",
            explanation: "Third person singular needs -s.",
            explanationTranslation: "3인칭 단수는 -s가 필요해요.",
          },
          {
            prompt: "She likes ___ books.",
            answer: "reading",
            translation: "그녀는 책 읽는 것을 좋아해요.",
            explanation: "The verb after \"likes\" takes the -ing form here.",
            explanationTranslation: "\"likes\" 다음에는 -ing 형태가 와요.",
          },
        ],
        quizStatus: "done",
      },
      turns: [{ turn: 1, role: "user", text: "hi", refined: false }],
    });
    const user = userEvent.setup();
    render(<App />);
    await user.click(await screen.findByText("hello there"));
    await screen.findByText("hi");

    await user.click(screen.getByRole("button", { name: "대화 종료" }));
    await user.click(await screen.findByText("퀴즈 풀기"));

    await screen.findByText("He ___ to school every day.");
    await user.type(screen.getByRole("textbox", { name: "정답 입력" }), "goes");
    await user.click(screen.getByRole("button", { name: "확인" }));
    await screen.findByText("정답이에요!");
    await user.click(screen.getByRole("button", { name: "다음 문제" }));
    await screen.findByText("She likes ___ books.");

    // Accidental outside click — dismisses the popover entirely.
    await user.click(document.body);
    expect(screen.queryByText("She likes ___ books.")).not.toBeInTheDocument();

    // Reopening must show question 2 again, not restart from question 1.
    await user.click(screen.getByRole("button", { name: "대화 종료" }));
    expect(await screen.findByText("She likes ___ books.")).toBeInTheDocument();
    expect(screen.queryByText("He ___ to school every day.")).not.toBeInTheDocument();
  });

  // Guards the "all correct" completion path this feature exists for: only
  // once every question in the quiz is answered correctly does it call
  // markQuizCompleted — a single wrong answer anywhere (covered above) must
  // never trigger it, and it must not fire after just the first of several
  // correct answers either.
  it("marks the quiz completed once every question is answered correctly", async () => {
    vi.mocked(fetchSessions).mockResolvedValue([
      { id: "s1", title: "hello there", createdAt: 1, updatedAt: 2, ended: true, studySummaryStatus: "done" },
    ]);
    vi.mocked(fetchSessionDetail).mockResolvedValue({
      hasMore: false,
      session: {
        id: "s1",
        title: "hello there",
        createdAt: 1,
        updatedAt: 2,
        ended: true,
        studySummary: [{ english: "Focus on third-person -s.", translation: "3인칭 단수 -s에 집중하세요." }],
        studySummaryStatus: "done",
        quiz: [
          {
            prompt: "He ___ to school every day.",
            answer: "goes",
            translation: "그는 매일 학교에 가요.",
            explanation: "Third person singular needs -s.",
            explanationTranslation: "3인칭 단수는 -s가 필요해요.",
          },
          {
            prompt: "She likes ___ books.",
            answer: "reading",
            translation: "그녀는 책 읽는 것을 좋아해요.",
            explanation: "The verb after \"likes\" takes the -ing form here.",
            explanationTranslation: "\"likes\" 다음에는 -ing 형태가 와요.",
          },
        ],
        quizStatus: "done",
      },
      turns: [{ turn: 1, role: "user", text: "hi", refined: false }],
    });
    const user = userEvent.setup();
    render(<App />);
    await user.click(await screen.findByText("hello there"));
    await screen.findByText("hi");

    await user.click(screen.getByRole("button", { name: "대화 종료" }));
    await user.click(await screen.findByText("퀴즈 풀기"));

    await screen.findByText("He ___ to school every day.");
    await user.type(screen.getByRole("textbox", { name: "정답 입력" }), "goes");
    await user.click(screen.getByRole("button", { name: "확인" }));
    await screen.findByText("정답이에요!");
    expect(markQuizCompleted).not.toHaveBeenCalled();

    await user.click(screen.getByRole("button", { name: "다음 문제" }));
    await screen.findByText("She likes ___ books.");
    await user.type(screen.getByRole("textbox", { name: "정답 입력" }), "reading");
    await user.click(screen.getByRole("button", { name: "확인" }));

    await vi.waitFor(() => expect(markQuizCompleted).toHaveBeenCalledWith("s1"));
  });

  // Guards the no-quiz-content case (see needsStudyQuizBackfill server-side):
  // instead of a dead-end "퀴즈 풀기" leading nowhere, the learner gets a
  // "내가 읽었음" button that marks the same completion checkmark a fully-
  // correct quiz would.
  it("shows an acknowledge button instead of the quiz when there's nothing to ask about", async () => {
    vi.mocked(fetchSessions).mockResolvedValue([
      { id: "s1", title: "hello there", createdAt: 1, updatedAt: 2, ended: true, studySummaryStatus: "done" },
    ]);
    vi.mocked(fetchSessionDetail).mockResolvedValue({
      hasMore: false,
      session: {
        id: "s1",
        title: "hello there",
        createdAt: 1,
        updatedAt: 2,
        ended: true,
        studySummary: [{ english: "Nice work, nothing to flag.", translation: "잘했어요, 지적할 부분이 없어요." }],
        studySummaryStatus: "done",
        quiz: [],
        quizStatus: "done",
      },
      turns: [{ turn: 1, role: "user", text: "hi", refined: false }],
    });
    const user = userEvent.setup();
    render(<App />);
    await user.click(await screen.findByText("hello there"));
    await screen.findByText("hi");

    await user.click(screen.getByRole("button", { name: "대화 종료" }));
    expect(screen.queryByText("퀴즈 풀기")).not.toBeInTheDocument();

    await user.click(await screen.findByText("내가 읽었음"));

    expect(markQuizCompleted).toHaveBeenCalledWith("s1");
    expect(await screen.findByText("학습 완료로 표시했어요.")).toBeInTheDocument();
  });

  // Guards the meaning hint (see QuizQuestion.answerMeaning): shown up front
  // alongside the blanked prompt, before the learner answers, since a
  // blanked English sentence alone could fit many different words — without
  // it there's nothing concrete to guess from.
  it("shows the answer's meaning hint before the learner answers", async () => {
    vi.mocked(fetchSessions).mockResolvedValue([
      { id: "s1", title: "hello there", createdAt: 1, updatedAt: 2, ended: true, studySummaryStatus: "done" },
    ]);
    vi.mocked(fetchSessionDetail).mockResolvedValue({
      hasMore: false,
      session: {
        id: "s1",
        title: "hello there",
        createdAt: 1,
        updatedAt: 2,
        ended: true,
        studySummary: [{ english: "Focus on third-person -s.", translation: "3인칭 단수 -s에 집중하세요." }],
        studySummaryStatus: "done",
        quiz: [
          {
            prompt: "He ___ to school every day.",
            answer: "goes",
            answerMeaning: "가다",
            translation: "그는 매일 학교에 가요.",
            explanation: "Third person singular needs -s.",
            explanationTranslation: "3인칭 단수는 -s가 필요해요.",
          },
        ],
        quizStatus: "done",
      },
      turns: [{ turn: 1, role: "user", text: "hi", refined: false }],
    });
    const user = userEvent.setup();
    render(<App />);
    await user.click(await screen.findByText("hello there"));
    await screen.findByText("hi");

    await user.click(screen.getByRole("button", { name: "대화 종료" }));
    await user.click(await screen.findByText("퀴즈 풀기"));

    expect(await screen.findByText("He ___ to school every day.")).toBeInTheDocument();
    expect(screen.getByText("가다", { exact: false })).toBeInTheDocument();
  });

  // Guards acceptableAnswers grading (see isQuizAnswerAccepted): a close
  // synonym the model already vetted as fitting this exact blank must be
  // graded correct, not just the one literal word in "answer".
  it("accepts a listed acceptable synonym as a correct answer", async () => {
    vi.mocked(fetchSessions).mockResolvedValue([
      { id: "s1", title: "hello there", createdAt: 1, updatedAt: 2, ended: true, studySummaryStatus: "done" },
    ]);
    vi.mocked(fetchSessionDetail).mockResolvedValue({
      hasMore: false,
      session: {
        id: "s1",
        title: "hello there",
        createdAt: 1,
        updatedAt: 2,
        ended: true,
        studySummary: [{ english: "Focus on third-person -s.", translation: "3인칭 단수 -s에 집중하세요." }],
        studySummaryStatus: "done",
        quiz: [
          {
            prompt: "He ___ to school every day.",
            answer: "goes",
            answerMeaning: "가다",
            acceptableAnswers: ["walks", "commutes"],
            translation: "그는 매일 학교에 가요.",
            explanation: "Third person singular needs -s.",
            explanationTranslation: "3인칭 단수는 -s가 필요해요.",
          },
        ],
        quizStatus: "done",
      },
      turns: [{ turn: 1, role: "user", text: "hi", refined: false }],
    });
    const user = userEvent.setup();
    render(<App />);
    await user.click(await screen.findByText("hello there"));
    await screen.findByText("hi");

    await user.click(screen.getByRole("button", { name: "대화 종료" }));
    await user.click(await screen.findByText("퀴즈 풀기"));

    await screen.findByText("He ___ to school every day.");
    await user.type(screen.getByRole("textbox", { name: "정답 입력" }), "  Walks. ");
    await user.click(screen.getByRole("button", { name: "확인" }));

    expect(await screen.findByText("정답이에요!")).toBeInTheDocument();
    await vi.waitFor(() => expect(markQuizCompleted).toHaveBeenCalledWith("s1"));
  });

  // Guards the LLM fallback (see checkQuizAnswer/isQuizAnswerAccepted): an
  // answer that matches neither `answer` nor `acceptableAnswers` literally
  // falls through to a server-side check rather than being marked wrong
  // outright — so a genuine synonym the quiz's own generation step didn't
  // think to list still counts as correct.
  it("falls back to checkQuizAnswer for an answer not already listed, and accepts it when the server confirms it", async () => {
    vi.mocked(checkQuizAnswer).mockResolvedValue(true);
    vi.mocked(fetchSessions).mockResolvedValue([
      { id: "s1", title: "hello there", createdAt: 1, updatedAt: 2, ended: true, studySummaryStatus: "done" },
    ]);
    vi.mocked(fetchSessionDetail).mockResolvedValue({
      hasMore: false,
      session: {
        id: "s1",
        title: "hello there",
        createdAt: 1,
        updatedAt: 2,
        ended: true,
        studySummary: [{ english: "Focus on third-person -s.", translation: "3인칭 단수 -s에 집중하세요." }],
        studySummaryStatus: "done",
        quiz: [
          {
            prompt: "He ___ to school every day.",
            answer: "goes",
            translation: "그는 매일 학교에 가요.",
            explanation: "Third person singular needs -s.",
            explanationTranslation: "3인칭 단수는 -s가 필요해요.",
          },
        ],
        quizStatus: "done",
      },
      turns: [{ turn: 1, role: "user", text: "hi", refined: false }],
    });
    const user = userEvent.setup();
    render(<App />);
    await user.click(await screen.findByText("hello there"));
    await screen.findByText("hi");

    await user.click(screen.getByRole("button", { name: "대화 종료" }));
    await user.click(await screen.findByText("퀴즈 풀기"));

    await screen.findByText("He ___ to school every day.");
    await user.type(screen.getByRole("textbox", { name: "정답 입력" }), "commutes");
    await user.click(screen.getByRole("button", { name: "확인" }));

    expect(checkQuizAnswer).toHaveBeenCalledWith("He ___ to school every day.", "goes", undefined, "commutes");
    expect(await screen.findByText("정답이에요!")).toBeInTheDocument();
    await vi.waitFor(() => expect(markQuizCompleted).toHaveBeenCalledWith("s1"));
  });

  // Guards the "biased toward wrong" default: when checkQuizAnswer resolves
  // false (the beforeEach default, matching quizAnswerCheckSystemPrompt's own
  // bias), an unlisted answer is graded wrong, same as before this fallback
  // existed.
  it("grades an unlisted answer wrong when checkQuizAnswer doesn't confirm it", async () => {
    vi.mocked(fetchSessions).mockResolvedValue([
      { id: "s1", title: "hello there", createdAt: 1, updatedAt: 2, ended: true, studySummaryStatus: "done" },
    ]);
    vi.mocked(fetchSessionDetail).mockResolvedValue({
      hasMore: false,
      session: {
        id: "s1",
        title: "hello there",
        createdAt: 1,
        updatedAt: 2,
        ended: true,
        studySummary: [{ english: "Focus on third-person -s.", translation: "3인칭 단수 -s에 집중하세요." }],
        studySummaryStatus: "done",
        quiz: [
          {
            prompt: "He ___ to school every day.",
            answer: "goes",
            translation: "그는 매일 학교에 가요.",
            explanation: "Third person singular needs -s.",
            explanationTranslation: "3인칭 단수는 -s가 필요해요.",
          },
        ],
        quizStatus: "done",
      },
      turns: [{ turn: 1, role: "user", text: "hi", refined: false }],
    });
    const user = userEvent.setup();
    render(<App />);
    await user.click(await screen.findByText("hello there"));
    await screen.findByText("hi");

    await user.click(screen.getByRole("button", { name: "대화 종료" }));
    await user.click(await screen.findByText("퀴즈 풀기"));

    await screen.findByText("He ___ to school every day.");
    await user.type(screen.getByRole("textbox", { name: "정답 입력" }), "banana");
    await user.click(screen.getByRole("button", { name: "확인" }));

    expect(await screen.findByText("아쉬워요. 정답: goes")).toBeInTheDocument();
    expect(markQuizCompleted).not.toHaveBeenCalled();
  });

  // Guards the "퀴즈 다시 만들기" reset button (see
  // httpserver.sessionQuizResetHandler): unlike restudyConversation, this is
  // available whenever a quiz already has real questions, not just a
  // stuck-empty one. Clicking it must call resetQuiz, show the same
  // "준비하는 중" state a still-generating quiz shows, and pick up the
  // regenerated quiz once pollQuizStatus's next tick sees it land.
  it("퀴즈 다시 만들기 resets the quiz and shows the regenerated questions once they land", async () => {
    vi.useFakeTimers();
    try {
      vi.mocked(fetchSessions).mockResolvedValue([
        { id: "s1", title: "hello there", createdAt: 1, updatedAt: 2, ended: true, studySummaryStatus: "done" },
      ]);
      vi.mocked(fetchSessionDetail).mockResolvedValue({
        hasMore: false,
        session: {
          id: "s1",
          title: "hello there",
          createdAt: 1,
          updatedAt: 2,
          ended: true,
          studySummary: [{ english: "Focus on third-person -s.", translation: "3인칭 단수 -s에 집중하세요." }],
          studySummaryStatus: "done",
          quiz: [{ prompt: "He ___ to school.", answer: "goes", translation: "그는 학교에 가요.", explanation: "old", explanationTranslation: "old" }],
          quizStatus: "done",
        },
        turns: [{ turn: 1, role: "user", text: "hi", refined: false }],
      });
      render(<App />);
      await flushUntil(() => screen.queryByText("hello there") !== null);
      fireEvent.click(screen.getByText("hello there"));
      await flushUntil(() => screen.queryByText("hi") !== null);

      fireEvent.click(screen.getByRole("button", { name: "대화 종료" }));
      await flushUntil(() => screen.queryByText("퀴즈 다시 만들기") !== null);

      fireEvent.click(screen.getByRole("button", { name: "퀴즈 다시 만들기" }));
      expect(resetQuiz).toHaveBeenCalledWith("s1");
      await flushUntil(() => screen.queryByText("퀴즈를 준비하는 중…") !== null);
      expect(screen.queryByText("퀴즈 풀기")).not.toBeInTheDocument();

      vi.mocked(fetchSessionDetail).mockResolvedValue({
        hasMore: false,
        session: {
          id: "s1",
          title: "hello there",
          createdAt: 1,
          updatedAt: 2,
          ended: true,
          studySummary: [{ english: "Focus on third-person -s.", translation: "3인칭 단수 -s에 집중하세요." }],
          studySummaryStatus: "done",
          quiz: [
            {
              prompt: "She ___ books.",
              answer: "reads",
              answerMeaning: "읽다",
              translation: "그녀는 책을 읽어요.",
              explanation: "new",
              explanationTranslation: "new",
            },
          ],
          quizStatus: "done",
        },
        turns: [{ turn: 1, role: "user", text: "hi", refined: false }],
      });
      await act(() => vi.advanceTimersByTimeAsync(4000));
      await flushUntil(() => screen.queryByText("퀴즈 풀기") !== null);
    } finally {
      vi.useRealTimers();
    }
  });

  // Guards the "다시 확인하기" force-recheck button (see
  // httpserver.sessionRestudyHandler): it must only appear alongside the "no
  // issues found" message, never alongside a wrap-up that already has real
  // content — that state already has "퀴즈 풀기" instead, and a stuck-empty
  // summary should never be silently overwritable by both buttons at once.
  it("shows the force-recheck button only when the study summary is empty", async () => {
    vi.mocked(fetchSessions).mockResolvedValue([
      { id: "s1", title: "hello there", createdAt: 1, updatedAt: 2, ended: true, studySummaryStatus: "done" },
    ]);
    vi.mocked(fetchSessionDetail).mockResolvedValue({
      hasMore: false,
      session: {
        id: "s1",
        title: "hello there",
        createdAt: 1,
        updatedAt: 2,
        ended: true,
        studySummary: [{ english: "Focus on third-person -s.", translation: "3인칭 단수 -s에 집중하세요." }],
        studySummaryStatus: "done",
      },
      turns: [{ turn: 1, role: "user", text: "hi", refined: false }],
    });
    const user = userEvent.setup();
    render(<App />);
    await user.click(await screen.findByText("hello there"));
    await screen.findByText("hi");
    await user.click(screen.getByRole("button", { name: "대화 종료" }));

    await screen.findByText("Focus on third-person -s.");
    expect(screen.queryByText("다시 확인하기")).not.toBeInTheDocument();
  });

  // Guards the recovery flow itself: clicking "다시 확인하기" while a wrap-up
  // is stuck reading "no issues found" must call restudySession (see
  // lib/sessions.ts), switch straight to the "정리 중" loading state, and
  // pick up the regenerated summary once a later poll sees it land — the
  // same pollStudySummary machinery a freshly-ended room already uses.
  it("다시 확인하기 forces the stuck wrap-up to regenerate and shows the result once it lands", async () => {
    vi.useFakeTimers();
    try {
      vi.mocked(fetchSessions).mockResolvedValue([
        { id: "s1", title: "hello there", createdAt: 1, updatedAt: 2, ended: true, studySummaryStatus: "done" },
      ]);
      vi.mocked(fetchSessionDetail).mockResolvedValue({
        hasMore: false,
        session: {
          id: "s1",
          title: "hello there",
          createdAt: 1,
          updatedAt: 2,
          ended: true,
          studySummary: [],
          studySummaryStatus: "done",
        },
        turns: [{ turn: 1, role: "user", text: "hi", refined: false }],
      });
      render(<App />);
      await flushUntil(() => screen.queryByText("hello there") !== null);
      fireEvent.click(screen.getByText("hello there"));
      await flushUntil(() => screen.queryByText("hi") !== null);

      fireEvent.click(screen.getByRole("button", { name: "대화 종료" }));
      await flushUntil(
        () => screen.queryByText("이번 대화에서는 딱히 걸린 부분이 없었어요. 아주 잘했어요!") !== null,
      );

      fireEvent.click(screen.getByRole("button", { name: "다시 확인하기" }));
      await flushUntil(() => screen.queryByText("학습 피드백을 정리하는 중…") !== null);
      expect(restudySession).toHaveBeenCalledWith("s1");

      vi.mocked(fetchSessionDetail).mockResolvedValue({
        hasMore: false,
        session: {
          id: "s1",
          title: "hello there",
          createdAt: 1,
          updatedAt: 2,
          ended: true,
          studySummary: [{ english: "Focus on third-person -s.", translation: "3인칭 단수 -s에 집중하세요." }],
          studySummaryStatus: "done",
        },
        turns: [{ turn: 1, role: "user", text: "hi", refined: false }],
      });
      await act(() => vi.advanceTimersByTimeAsync(4000));
      await flushUntil(() => screen.queryByText("Focus on third-person -s.") !== null);
    } finally {
      vi.useRealTimers();
    }
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

  it("navigates to the relative word-matching game page from the menu", async () => {
    const user = userEvent.setup();
    render(<App />);
    await openMenu(user);
    await user.click(screen.getByRole("menuitem", { name: /단어 매칭 게임/ }));
    expect(location.assign).toHaveBeenCalledWith("match");
  });
});

describe("conversation style", () => {
  it("populates the field with the saved style once loaded", async () => {
    vi.mocked(fetchSettings).mockResolvedValue({
      interlocutorStyle: "면접관처럼 질문해줘",
      learnerProfile: "",
    });
    const user = userEvent.setup();
    render(<App />);
    await openMenu(user);
    expect(await screen.findByLabelText("대화 상대 스타일")).toHaveValue("면접관처럼 질문해줘");
  });

  it("saves the edited style and shows a confirmation", async () => {
    vi.mocked(fetchSettings).mockResolvedValue({ interlocutorStyle: "", learnerProfile: "" });
    const user = userEvent.setup();
    render(<App />);
    await openMenu(user);
    await user.type(screen.getByLabelText("대화 상대 스타일"), "전문가처럼 답변해줘");
    await user.click(screen.getByRole("button", { name: "저장" }));
    expect(saveSettings).toHaveBeenCalledWith("전문가처럼 답변해줘");
    expect(await screen.findByText("저장됨")).toBeInTheDocument();
  });

  // Regression: fetchSettings used to fall back to an empty style on any
  // load failure, indistinguishable from a learner who never set one — a
  // blank field looked "reset" even though the real value was still saved
  // server-side, and submitting over it would have actually erased it. A
  // failed load must now disable the form instead of silently showing blank.
  it("disables the form instead of showing a false-empty style on load failure", async () => {
    vi.mocked(fetchSettings).mockResolvedValue(null);
    const user = userEvent.setup();
    render(<App />);
    await openMenu(user);
    expect(await screen.findByText(/불러오지 못했습니다/)).toBeInTheDocument();
    expect(screen.getByLabelText("대화 상대 스타일")).toBeDisabled();
    expect(screen.getByRole("button", { name: "저장" })).toBeDisabled();
    expect(saveSettings).not.toHaveBeenCalled();
  });

  // Regression: submitting a style over the server's 500-char cap used to
  // fail the PUT with a 400 that saveSettings collapsed into a bare `false`,
  // which submitStyle then did nothing with — no error, just no "저장됨".
  // The limit is now checked client-side so the learner gets a specific
  // message instead of a silent no-op.
  it("rejects a style over the length limit without calling saveSettings", async () => {
    vi.mocked(fetchSettings).mockResolvedValue({ interlocutorStyle: "", learnerProfile: "" });
    const user = userEvent.setup();
    render(<App />);
    await openMenu(user);
    const textarea = screen.getByLabelText("대화 상대 스타일");
    fireEvent.change(textarea, { target: { value: "a".repeat(1025) } });
    await user.click(screen.getByRole("button", { name: "저장" }));
    expect(await screen.findByText("1024자를 초과했습니다 (현재 1025자).")).toBeInTheDocument();
    expect(saveSettings).not.toHaveBeenCalled();
    expect(screen.queryByText("저장됨")).not.toBeInTheDocument();
  });

  it("shows an error instead of failing silently when the save request fails", async () => {
    vi.mocked(fetchSettings).mockResolvedValue({ interlocutorStyle: "", learnerProfile: "" });
    vi.mocked(saveSettings).mockResolvedValue(false);
    const user = userEvent.setup();
    render(<App />);
    await openMenu(user);
    await user.type(screen.getByLabelText("대화 상대 스타일"), "전문가처럼 답변해줘");
    await user.click(screen.getByRole("button", { name: "저장" }));
    expect(await screen.findByText("저장하지 못했습니다. 다시 시도해주세요.")).toBeInTheDocument();
    expect(screen.queryByText("저장됨")).not.toBeInTheDocument();
  });
});

describe("learner profile", () => {
  it("stays collapsed until the toggle button is clicked", async () => {
    vi.mocked(fetchSettings).mockResolvedValue({
      interlocutorStyle: "",
      learnerProfile: "articles를 자주 틀림; 요리에 관심 많음",
    });
    const user = userEvent.setup();
    render(<App />);
    await openMenu(user);
    expect(screen.queryByText("articles를 자주 틀림; 요리에 관심 많음")).not.toBeInTheDocument();
    await user.click(screen.getByRole("button", { name: "🧠 학습 프로필 보기" }));
    expect(await screen.findByText("articles를 자주 틀림; 요리에 관심 많음")).toBeInTheDocument();
  });

  it("shows an empty state when no session has ended yet", async () => {
    vi.mocked(fetchSettings).mockResolvedValue({ interlocutorStyle: "", learnerProfile: "" });
    const user = userEvent.setup();
    render(<App />);
    await openMenu(user);
    await user.click(screen.getByRole("button", { name: "🧠 학습 프로필 보기" }));
    expect(await screen.findByText(/아직 정리된 내용이 없어요/)).toBeInTheDocument();
  });

  it("shows the load-failure error when opened after a failed settings fetch", async () => {
    vi.mocked(fetchSettings).mockResolvedValue(null);
    const user = userEvent.setup();
    render(<App />);
    await openMenu(user);
    await user.click(screen.getByRole("button", { name: "🧠 학습 프로필 보기" }));
    expect(await screen.findAllByText(/불러오지 못했습니다/)).toHaveLength(2);
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

  it("does not re-fetch when closed, only when reopened", async () => {
    openRoomS1();
    vi.mocked(fetchSessionCompaction).mockResolvedValue({
      summary: "summary",
      recentMessages: 1,
      totalTurns: 2,
    });
    const user = userEvent.setup();
    render(<App />);
    await user.click(await screen.findByText("hello there"));
    await screen.findByText("hi");

    const toggle = screen.getByRole("button", { name: "대화 압축 상태 보기" });
    await user.click(toggle); // open: fetches
    expect(fetchSessionCompaction).toHaveBeenCalledTimes(1);

    await user.click(toggle); // close: must not fetch again
    expect(fetchSessionCompaction).toHaveBeenCalledTimes(1);

    await user.click(toggle); // reopen: fetches again
    expect(fetchSessionCompaction).toHaveBeenCalledTimes(2);
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

    expect(lastSendText()).toHaveBeenCalledWith("Hello Buddy", undefined);
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

// The word-lookup panel is anchored next to the composer (see
// WordSearchControl in App.tsx), not the header — reachable mid-typing
// without ever navigating away from the chat window, unlike the header
// popovers covered under "compaction info"/"feedback summary" above.
describe("word search panel", () => {
  async function openExistingRoom(user: ReturnType<typeof userEvent.setup>, title = "word test room") {
    vi.mocked(fetchSessions).mockResolvedValue([{ id: "s1", title, createdAt: 1, updatedAt: 2 }]);
    vi.mocked(fetchSessionDetail).mockResolvedValue({
      hasMore: false,
      session: { id: "s1", title, createdAt: 1, updatedAt: 2 },
      turns: [],
    });
    render(<App />);
    await user.click(await screen.findByText(title));
    await screen.findByPlaceholderText("…or type in English");
  }

  it("lets the learner describe a word in Korean and shows the LLM's English suggestions", async () => {
    vi.mocked(suggestWords).mockResolvedValue([
      { word: "furious", meaning: "화가 나서 참을 수 없는", example: "She was furious when she found out." },
    ]);
    const user = userEvent.setup();
    await openExistingRoom(user);

    await user.click(screen.getByRole("button", { name: "모르는 단어 찾기" }));
    await user.type(
      screen.getByPlaceholderText("예: 화가 나서 참을 수 없는 느낌"),
      "화가 나서 참을 수 없는 느낌",
    );
    await user.click(screen.getByRole("button", { name: "찾기" }));

    expect(suggestWords).toHaveBeenCalledWith("화가 나서 참을 수 없는 느낌");
    expect(await screen.findByText("furious")).toBeInTheDocument();
    expect(screen.getByText("She was furious when she found out.")).toBeInTheDocument();
  });

  it("shows a failure message when the request fails", async () => {
    vi.mocked(suggestWords).mockResolvedValue(null);
    const user = userEvent.setup();
    await openExistingRoom(user);

    await user.click(screen.getByRole("button", { name: "모르는 단어 찾기" }));
    await user.type(screen.getByPlaceholderText("예: 화가 나서 참을 수 없는 느낌"), "설명");
    await user.click(screen.getByRole("button", { name: "찾기" }));

    expect(await screen.findByText("불러오지 못했어요.")).toBeInTheDocument();
  });

  // The learner closes the panel to go back to writing their own sentence
  // in the composer while a (possibly slow, see wordSearch.ts) search is
  // still in flight — the icon button must surface completion without
  // forcing the panel to stay open and block their typing.
  it("badges the search icon once a search finishes while the panel is closed", async () => {
    let resolveSearch: (value: Awaited<ReturnType<typeof suggestWords>>) => void = () => {};
    vi.mocked(suggestWords).mockReturnValue(
      new Promise((resolve) => {
        resolveSearch = resolve;
      }),
    );
    const user = userEvent.setup();
    await openExistingRoom(user);

    await user.click(screen.getByRole("button", { name: "모르는 단어 찾기" }));
    await user.type(screen.getByPlaceholderText("예: 화가 나서 참을 수 없는 느낌"), "화가 나서");
    await user.click(screen.getByRole("button", { name: "찾기" }));

    await user.click(screen.getByRole("button", { name: "모르는 단어 찾기" }));
    expect(screen.queryByPlaceholderText("예: 화가 나서 참을 수 없는 느낌")).not.toBeInTheDocument();

    resolveSearch([{ word: "furious", meaning: "화가 나서 참을 수 없는", example: "She was furious." }]);
    await screen.findByRole("button", { name: "모르는 단어 찾기 (검색 결과 도착)" });

    await user.click(screen.getByRole("button", { name: "모르는 단어 찾기 (검색 결과 도착)" }));
    expect(await screen.findByText("furious")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "모르는 단어 찾기" })).toBeInTheDocument();
  });

  // The whole point of anchoring this next to the composer instead of a
  // separate screen: opening it must never unmount the composer itself.
  it("stays inside the chat window — the composer remains mounted while the panel is open", async () => {
    const user = userEvent.setup();
    await openExistingRoom(user);

    await user.click(screen.getByRole("button", { name: "모르는 단어 찾기" }));
    expect(screen.getByPlaceholderText("…or type in English")).toBeInTheDocument();
  });

  // "학습하기" only saves the one suggestion the learner explicitly picks —
  // opening the panel and searching must never bulk-save every result (see
  // wordreview's package doc and httpserver.wordSaveHandler).
  it("adds only the chosen suggestion to the study list, not the whole search result", async () => {
    vi.mocked(suggestWords).mockResolvedValue([
      { word: "furious", meaning: "화가 나서 참을 수 없는", example: "She was furious." },
      { word: "livid", meaning: "몹시 화가 난", example: "He was livid." },
    ]);
    vi.mocked(saveWord).mockResolvedValue({
      id: "w1",
      word: "furious",
      meaning: "화가 나서 참을 수 없는",
      example: "She was furious.",
      stage: 0,
      reviewCount: 0,
      nextReviewAt: 1700000000,
      status: "pending",
    });
    const user = userEvent.setup();
    await openExistingRoom(user);

    await user.click(screen.getByRole("button", { name: "모르는 단어 찾기" }));
    await user.type(screen.getByPlaceholderText("예: 화가 나서 참을 수 없는 느낌"), "화가 나서");
    await user.click(screen.getByRole("button", { name: "찾기" }));
    await screen.findByText("furious");

    const learnButtons = screen.getAllByRole("button", { name: "학습하기" });
    expect(learnButtons).toHaveLength(2);
    await user.click(learnButtons[0]);

    expect(saveWord).toHaveBeenCalledTimes(1);
    expect(saveWord).toHaveBeenCalledWith({
      word: "furious",
      meaning: "화가 나서 참을 수 없는",
      example: "She was furious.",
    });
    // The clicked suggestion flips to a disabled "still verifying" state; the
    // other suggestion is untouched and still offers "학습하기".
    expect(await screen.findByRole("button", { name: "✓ 확인 중" })).toBeDisabled();
    expect(screen.getByRole("button", { name: "학습하기" })).toBeInTheDocument();
  });

  // Regression test for independent per-meaning tracking: two suggestions
  // that share a word but differ in meaning (e.g. "bank" the riverbank vs.
  // "bank" the financial one) must track "학습하기"/"확인 중" independently —
  // saving one must never disable the other's button.
  it("tracks the same word with two different meanings independently", async () => {
    vi.mocked(suggestWords).mockResolvedValue([
      { word: "bank", meaning: "강둑", example: "They sat on the bank." },
      { word: "bank", meaning: "은행", example: "I went to the bank." },
    ]);
    vi.mocked(saveWord).mockResolvedValue({
      id: "w1",
      word: "bank",
      meaning: "강둑",
      example: "They sat on the bank.",
      stage: 0,
      reviewCount: 0,
      nextReviewAt: 1700000000,
      status: "pending",
    });
    const user = userEvent.setup();
    await openExistingRoom(user);

    await user.click(screen.getByRole("button", { name: "모르는 단어 찾기" }));
    await user.type(screen.getByPlaceholderText("예: 화가 나서 참을 수 없는 느낌"), "은행 강둑");
    await user.click(screen.getByRole("button", { name: "찾기" }));
    await screen.findByText("강둑");

    const learnButtons = screen.getAllByRole("button", { name: "학습하기" });
    expect(learnButtons).toHaveLength(2);
    await user.click(learnButtons[0]);

    expect(saveWord).toHaveBeenCalledWith({ word: "bank", meaning: "강둑", example: "They sat on the bank." });
    // Only the riverbank sense flips to "확인 중" — the financial sense (same
    // word text, different meaning) must still offer "학습하기".
    expect(await screen.findByRole("button", { name: "✓ 확인 중" })).toBeDisabled();
    expect(screen.getByRole("button", { name: "학습하기" })).toBeEnabled();
  });

  it("leaves the suggestion clickable again if saving it fails", async () => {
    vi.mocked(suggestWords).mockResolvedValue([
      { word: "furious", meaning: "화가 나서 참을 수 없는", example: "She was furious." },
    ]);
    vi.mocked(saveWord).mockResolvedValue(null);
    const user = userEvent.setup();
    await openExistingRoom(user);

    await user.click(screen.getByRole("button", { name: "모르는 단어 찾기" }));
    await user.type(screen.getByPlaceholderText("예: 화가 나서 참을 수 없는 느낌"), "화가 나서");
    await user.click(screen.getByRole("button", { name: "찾기" }));
    await screen.findByText("furious");

    await user.click(screen.getByRole("button", { name: "학습하기" }));

    expect(saveWord).toHaveBeenCalledTimes(1);
    expect(await screen.findByRole("button", { name: "학습하기" })).toBeEnabled();
  });
});

// Opening the word-search panel (or switching rooms) must never silently
// drop what a learner was mid-typing — see lib/draftCache.ts.
describe("composer draft caching", () => {
  it("persists the in-progress draft per room and restores it on reopen", async () => {
    vi.mocked(fetchSessions).mockResolvedValue([
      { id: "s1", title: "draft room", createdAt: 1, updatedAt: 2 },
    ]);
    vi.mocked(fetchSessionDetail).mockResolvedValue({
      hasMore: false,
      session: { id: "s1", title: "draft room", createdAt: 1, updatedAt: 2 },
      turns: [],
    });
    const user = userEvent.setup();
    render(<App />);
    await user.click(await screen.findByText("draft room"));
    const textarea = await screen.findByPlaceholderText("…or type in English");
    await user.type(textarea, "half-typed sentence");

    expect(localStorage.getItem("buddy.chat.draft.s1")).toContain("half-typed sentence");

    await user.click(screen.getByRole("button", { name: "목록으로" }));
    act(() => capturedPopStateHandler?.({ view: "list" }));
    await screen.findByRole("button", { name: "+ 새 대화" });

    await user.click(await screen.findByText("draft room"));
    expect(await screen.findByPlaceholderText("…or type in English")).toHaveValue("half-typed sentence");
  });

  it("clears the cached draft once the message is actually sent", async () => {
    vi.mocked(fetchSessions).mockResolvedValue([
      { id: "s1", title: "draft room", createdAt: 1, updatedAt: 2 },
    ]);
    vi.mocked(fetchSessionDetail).mockResolvedValue({
      hasMore: false,
      session: { id: "s1", title: "draft room", createdAt: 1, updatedAt: 2 },
      turns: [],
    });
    const user = userEvent.setup();
    render(<App />);
    await user.click(await screen.findByText("draft room"));
    const textarea = await screen.findByPlaceholderText("…or type in English");
    await user.type(textarea, "send me{Enter}");

    expect(localStorage.getItem("buddy.chat.draft.s1")).toBeNull();
  });
});

// A spoken utterance no longer sends itself: the server proposes a
// still-editable guess ("pending_transcript") and the learner has to review
// and actually hit Send — guarding against STT hallucinating words never
// said, which used to burn a reply/correction/TTS call on every mis-hearing.
describe("voice draft confirmation funnel", () => {
  function lastSendText() {
    const mocked = vi.mocked(BuddyClient);
    const instance = mocked.mock.results[mocked.mock.results.length - 1].value as {
      sendText: ReturnType<typeof vi.fn>;
    };
    return instance.sendText;
  }

  it("drops a pending_transcript draft into the composer instead of sending it", async () => {
    const user = userEvent.setup();
    render(<App />);
    await enterNewChat(user);

    act(() => emit({ type: "pending_transcript", turn: 0, text: "i are hungry", source: "voice" }));

    expect(screen.getByPlaceholderText("…or type in English")).toHaveValue("i are hungry");
    expect(lastSendText()).not.toHaveBeenCalled();
  });

  it("sends the confirmed draft tagged with voice source", async () => {
    const user = userEvent.setup();
    render(<App />);
    await enterNewChat(user);
    act(() => emit({ type: "pending_transcript", turn: 0, text: "i are hungry", source: "voice" }));

    await user.click(screen.getByRole("button", { name: "Send" }));

    expect(lastSendText()).toHaveBeenCalledWith("i are hungry", "voice");
    expect(screen.getByPlaceholderText("…or type in English")).toHaveValue("");
  });

  it("upgrades an untouched draft when a slower (Judge-reconciled) guess arrives", async () => {
    const user = userEvent.setup();
    render(<App />);
    await enterNewChat(user);
    act(() => emit({ type: "pending_transcript", turn: 0, text: "i are hungry", source: "voice" }));

    act(() => emit({ type: "pending_transcript", turn: 0, text: "I am hungry", source: "voice" }));

    expect(screen.getByPlaceholderText("…or type in English")).toHaveValue("I am hungry");
  });

  it("does not clobber a learner's own edit with a later pending_transcript upgrade", async () => {
    const user = userEvent.setup();
    render(<App />);
    await enterNewChat(user);
    act(() => emit({ type: "pending_transcript", turn: 0, text: "i are hungry", source: "voice" }));

    const textarea = screen.getByPlaceholderText("…or type in English");
    await user.type(textarea, " a lot"); // the learner tweaks the draft themselves

    act(() => emit({ type: "pending_transcript", turn: 0, text: "I am hungry", source: "voice" }));

    expect(textarea).toHaveValue("i are hungry a lot");
  });

  it("discards the draft via the cancel button", async () => {
    const user = userEvent.setup();
    render(<App />);
    await enterNewChat(user);
    act(() => emit({ type: "pending_transcript", turn: 0, text: "i are hungry", source: "voice" }));

    await user.click(screen.getByRole("button", { name: "음성 초안 취소" }));
    const textarea = screen.getByPlaceholderText("…or type in English");
    expect(textarea).toHaveValue("");

    await user.type(textarea, "hello");
    await user.click(screen.getByRole("button", { name: "Send" }));
    expect(lastSendText()).toHaveBeenCalledWith("hello", undefined);
  });

  it("discards an unsent draft when a new recording starts", async () => {
    const user = userEvent.setup();
    render(<App />);
    await enterNewChat(user);
    act(() => emit({ type: "pending_transcript", turn: 0, text: "i are hungry", source: "voice" }));

    await user.click(screen.getByRole("button", { name: "Push to talk" }));

    expect(screen.getByPlaceholderText("…or type in English")).toHaveValue("");
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
      await flushUntil(() => screen.queryByText("hello there") !== null);
      fireEvent.click(screen.getByText("hello there"));
      await flushUntil(() => screen.queryByText("hi") !== null);
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
      await act(() => vi.advanceTimersByTimeAsync(4000));
      await flushUntil(() => screen.queryByText("recovered on another replica") !== null);

      expect(screen.getByText("recovered on another replica")).toBeInTheDocument();
      expect(screen.queryByRole("status", { name: "답변 생성 중" })).not.toBeInTheDocument();
    } finally {
      vi.useRealTimers();
    }
  });

  // The poll above keeps rescheduling itself every 4s for as long as work is
  // outstanding (up to maxAttempts), and a scheduled tick is not tied to the
  // component in any way — so unmounting has to cancel it explicitly. This
  // is also what made the test above flake in CI: with real timers, the
  // previous test's still-armed tick would fire in the middle of a later
  // test, fetch, and eat that test's mockResolvedValueOnce — leaving the
  // real enterChat fetch to fall through to the beforeEach default (null),
  // which sends the app straight back to the room list, so the transcript
  // it was waiting on never rendered. Only reproduced under CI's slower
  // scheduling, where a whole test can take longer than the 4s interval.
  it("stops polling once the app unmounts", async () => {
    vi.useFakeTimers();
    try {
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
      render(<App />);
      await flushUntil(() => screen.queryByText("hello there") !== null);
      fireEvent.click(screen.getByText("hello there"));
      await flushUntil(() => screen.queryByText("hi") !== null);

      const callsAtUnmount = vi.mocked(fetchSessionDetail).mock.calls.length;
      cleanup();
      // Several poll intervals' worth — one tick alone could be missed by
      // luck, and the reschedule is as much of a leak as the first timer.
      await act(() => vi.advanceTimersByTimeAsync(20000));

      expect(vi.mocked(fetchSessionDetail).mock.calls.length).toBe(callsAtUnmount);
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
