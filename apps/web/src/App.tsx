import { useCallback, useEffect, useLayoutEffect, useMemo, useRef, useState } from "react";
import { BuddyClient, type Status } from "./lib/ws";
import type {
  Correction,
  QuizQuestion,
  ServerEvent,
  StudySummarySentence,
} from "./lib/protocol";
import { PCMRecorder } from "./audio/recorder";
import { requestAmbientAudioSession } from "./lib/audioSession";
import { prPath } from "./lib/rootPath";
import { useDismiss } from "./hooks/useDismiss";
import { usePollScaffold } from "./hooks/usePollScaffold";
import { useUserPreferences } from "./hooks/useUserPreferences";
import {
  currentRoomHistoryState,
  goBack,
  onRoomPopState,
  parseRoomHash,
  pushRoomState,
  replaceRoomState,
} from "./lib/roomHistory";
import {
  deleteSession,
  endSession,
  fetchSessionDetail,
  fetchSessions,
  markCorrectionRead,
  markInstant,
  messageAudioURL,
  resetQuiz,
  restudySession,
  type SessionDetail,
  type SessionSummary,
} from "./lib/sessions";
import { clearDraft, loadDraft, saveDraft } from "./lib/draftCache";
import {
  correctionHasIssues,
  hydrateTurnMeta,
  isPendingPlaceholder,
  turnsToMsgs,
  upsertAssistant,
  type Msg,
  type TurnMeta,
} from "./lib/turns";
import { TopBar } from "./components/TopBar";
import { CompactionInfo } from "./components/CompactionInfo";
import { EndConversationControl } from "./components/EndConversationControl";
import { FeedbackSummary } from "./components/FeedbackSummary";
import { SessionList } from "./components/SessionList";
import { ConversationTranscript } from "./components/ConversationTranscript";
import { MessageComposer } from "./components/MessageComposer";

type View = "list" | "chat";

// How many turns enterChat loads up front, and how many more loadOlderTurns
// fetches per scroll-to-top request — mirrors the server's
// defaultSessionPageLimit (apps/server/internal/httpserver/server.go).
const HISTORY_PAGE_SIZE = 30;
// How close to the top/bottom edge (px) of .convo counts as "there" for
// triggering loadOlderTurns and for auto-scrolling to newly arrived
// messages, respectively.
const SCROLL_EDGE_THRESHOLD = 80;

const CONNECTION_LABELS: Record<Status, string> = {
  connecting: "대화에 연결하는 중",
  open: "대화에 연결됨",
  closed: "연결이 끊김 — 다시 연결하는 중",
  error: "연결 오류 — 다시 연결하는 중",
};

export function App() {
  const {
    email,
    theme,
    selectTheme,
    autoReadAloud,
    setAutoReadAloud,
    autoReadAloudRef,
    playbackRate,
    setPlaybackRate,
    playbackRateRef,
    styleInput,
    styleSaving,
    styleSaved,
    styleLoadError,
    styleSaveError,
    learnerProfile,
    wordDueCount,
    submitStyle,
    handleStyleInputChange,
  } = useUserPreferences();

  // The home screen lands on the room list by default; a refresh while a
  // room is open restores that room instead, from the URL hash (see the
  // mount effect below) — but a WS connection is still never silently
  // reconnected without re-hydrating the transcript first (see enterChat).
  const [view, setView] = useState<View>("list");
  const [sessions, setSessions] = useState<SessionSummary[]>([]);
  // Loading and failure are deliberately separate from `sessions`: an API
  // outage must not masquerade as "you have no conversations", and a quiet
  // background refresh failure should leave already-visible rooms intact.
  const [sessionsLoading, setSessionsLoading] = useState(true);
  const [sessionsLoadError, setSessionsLoadError] = useState(false);
  // Errors from an individual list action (open/delete), shown inline with
  // the list instead of failing silently while the row appears unchanged.
  const [listActionError, setListActionError] = useState<string | null>(null);
  const [deletingSessionId, setDeletingSessionId] = useState<string | null>(null);
  const [openingSessionId, setOpeningSessionId] = useState<string | null>(null);
  // The open room's id, once known — null for a brand-new room until the
  // server mints one (see the "ready" case in onEvent below). Only needed so
  // CompactionInfo has something to fetch against; the WS client and history
  // entry each track their own copy of this for their own purposes.
  const [activeSessionId, setActiveSessionId] = useState<string | null>(null);
  // onEvent (below) is a permanently-stable useCallback and needs to read
  // this synchronously (to build the auto-read-aloud URL for a just-arrived
  // reply), so it's kept in a ref alongside the state used for rendering,
  // same reasoning as quickModeRef.
  const activeSessionIdRef = useRef<string | null>(null);
  useEffect(() => {
    activeSessionIdRef.current = activeSessionId;
  }, [activeSessionId]);
  const [status, setStatus] = useState<Status>("connecting");
  const [msgs, setMsgs] = useState<Msg[]>([]);
  // True once the open room's fetched detail reports it as permanently
  // ended (see endSession) — no WS connection is kept for it, and the
  // composer is disabled; the room is otherwise browsable read-only.
  // Reset in both enterChat (about to load a possibly-different room) and
  // resetToListView (leaving the room entirely), same lifecycle as
  // resetTurnState.
  const [ended, setEnded] = useState(false);
  // Mirrors `ended` for onEvent (a useCallback whose identity/closure is
  // captured once at the WS client's construction — see the effect that
  // builds clientRef.current) to read synchronously without needing `ended`
  // in its dependency array, same reasoning as stickToBottomRef below.
  const endedRef = useRef(false);
  // The persisted wrap-up EndConversationControl shows for an ended room —
  // set alongside `ended` in enterChat from whatever SessionDetail already
  // reports, and refreshed by pollStudySummary below while it's still being
  // generated.
  const [endedSummary, setEndedSummary] = useState<StudySummarySentence[]>([]);
  // Mirrors store.SessionMeta.StudySummaryStatus for the open room — "done"
  // is the resting state (nothing left to poll for, including a room ended
  // before this became an async job, whose summary was already generated
  // synchronously — see SessionSummary.studySummaryStatus's doc comment).
  // Sampled fresh in enterChat, then kept current by pollStudySummary while
  // "pending"/"failed" so the room shows its wrap-up as soon as the
  // background job finishes, even if that's well after this room was
  // opened.
  const [endedSummaryStatus, setEndedSummaryStatus] = useState<"pending" | "done" | "failed">("done");
  // Mirrors endedSummary/endedSummaryStatus, but for the pre-generated
  // practice quiz (see store.SessionMeta.Quiz/QuizStatus) — generated
  // alongside the wrap-up, from the same flagged issues, right when the
  // conversation ends, so EndConversationControl's "퀴즈 풀기" button reads
  // this instead of triggering an LLM call itself. Sampled fresh in
  // enterChat, then kept current by pollQuizStatus while "pending"/"failed".
  const [endedQuiz, setEndedQuiz] = useState<QuizQuestion[]>([]);
  const [endedQuizStatus, setEndedQuizStatus] = useState<"pending" | "done" | "failed">("done");
  // Mirrors store.SessionMeta.QuizCompleted for the open room — a one-way
  // "studied this" checkmark (see markQuizCompleted), sampled fresh in
  // enterChat. Never polled for like the two above: it only ever changes
  // because this client itself called markQuizCompleted, so there's nothing
  // else to wait on landing in the background.
  const [endedQuizCompleted, setEndedQuizCompleted] = useState(false);
  // Grammar/translation state per turn — pending flags are set the moment a
  // result is expected (final_transcript/assistant_done for a live turn, or
  // on hydration for a history turn still missing one, see enterChat/
  // pollMissingFeedback) so leaving and reopening a room doesn't just
  // silently drop the spinner. Rendering always prefers actual translation
  // text over its pending flag (see the message list below), so a landed
  // translation doesn't need to explicitly clear it; a landed correction
  // does (see GrammarControl, which shows the pending spinner regardless of
  // whether a correction is already present).
  const [turns, setTurns] = useState<Record<number, TurnMeta>>({});

  const patchTurn = useCallback((turn: number, patch: Partial<TurnMeta>) => {
    setTurns((prev) => ({ ...prev, [turn]: { ...prev[turn], ...patch } }));
  }, []);

  // Merges a batch of per-turn patches (e.g. from polling several turns at
  // once) into one state update instead of one setTurns call per turn.
  const patchTurns = useCallback((patches: Record<number, Partial<TurnMeta>>) => {
    setTurns((prev) => {
      const next = { ...prev };
      for (const [turnStr, patch] of Object.entries(patches)) {
        const turn = Number(turnStr);
        next[turn] = { ...next[turn], ...patch };
      }
      return next;
    });
  }, []);

  // True from the moment a reply is expected (a message was just sent, or a
  // brand-new room was just opened and the server is about to volunteer its
  // opening line) until the first token of that reply arrives — drives the
  // typing indicator in the empty gap before assistant_delta/assistant_done.
  const [awaitingReply, setAwaitingReply] = useState(false);

  // Clears per-turn UI state (corrections, translations) — shared by
  // enterChat (about to load a room's own state, or none for a fresh one)
  // and backToList (leaving the room entirely).
  const resetTurnState = useCallback(() => {
    setTurns({});
  }, []);

  // Clears the ended-room wrap-up/quiz state — shared by enterChat (about to
  // load a room's own state, or none for a fresh one) and resetToListView
  // (leaving the room entirely).
  const resetEndedState = useCallback(() => {
    setEnded(false);
    endedRef.current = false;
    setEndedSummary([]);
    setEndedSummaryStatus("done");
    setEndedQuiz([]);
    setEndedQuizStatus("done");
    setEndedQuizCompleted(false);
    setAwaitingReply(false);
  }, []);

  // True for a room opened via "인스턴트 대화" (see enterChat's `quick` opt) —
  // one exchange, then the room ends itself, no manual "종료" tap needed, and
  // it's marked instant (see markInstant below) so it never shows up in the
  // main room list — only in its own list, pages/InstantSessions.tsx. Usable
  // any number of times, not gated to once a day — "quick"/instant just
  // describes the one-exchange-then-done shape of the room itself. Mirrors
  // endedRef's reasoning: onEvent's assistant_done handler is a stable
  // useCallback and needs to read this synchronously, so it's kept in a ref
  // alongside the state used for rendering.
  const [quickMode, setQuickMode] = useState(false);
  const quickModeRef = useRef(false);
  // The turn number of the one exchange a quick-mode room is waiting on —
  // set once (assistant_done for turn >= 1; turn 0 is the room's own opening
  // greeting, not the learner's sentence) so the effect below knows which
  // turn's correction/translations to wait for before auto-ending.
  const [quickWatchTurn, setQuickWatchTurn] = useState<number | null>(null);
  // True once the learner has sent their one sentence in quick mode — locks
  // the composer immediately (see the footer below) so a second message
  // can't sneak in before the auto-end effect has had a chance to fire.
  const [quickSent, setQuickSent] = useState(false);
  // Guards the auto-end effect below against firing endSession more than
  // once (e.g. a later unrelated `turns` update re-running the effect while
  // the first endSession call is still in flight).
  const quickEndingRef = useRef(false);

  const resetQuickState = useCallback((quick: boolean) => {
    quickModeRef.current = quick;
    setQuickMode(quick);
    setQuickWatchTurn(null);
    setQuickSent(false);
    quickEndingRef.current = false;
  }, []);

  const [mic, setMic] = useState(false);
  const [text, setText] = useState("");
  // True while the composer's text is a still-unsent voice draft (an
  // EvPendingTranscript guess the learner hasn't sent yet) rather than typed
  // input — determines the "source" tag on the eventual send, and lets the
  // UI show it's awaiting review instead of already part of the conversation.
  const [voiceDraft, setVoiceDraft] = useState(false);
  // The last text this component itself wrote into the composer from a
  // pending_transcript event — compared against the live `text` state so a
  // later (Judge-upgraded) pending_transcript never clobbers an edit the
  // learner already started making. Reset (to null) whenever the draft is
  // sent, discarded, or a new recording starts.
  const voiceDraftAutoTextRef = useRef<string | null>(null);
  // True while a spoken utterance has been sent to the server but no
  // pending_transcript (or error) has come back for it yet.
  const [transcribing, setTranscribing] = useState(false);
  // User-facing failures in the live conversation path (microphone, STT,
  // TTS). These used to be console-only, which made a failed tap look like
  // the app had simply ignored it.
  const [chatError, setChatError] = useState<string | null>(null);
  const [menuOpen, setMenuOpen] = useState(false);
  // Which message row's grammar-feedback popover is open, or null — only one
  // open at a time. Read-aloud no longer has a popover of its own (see
  // StudyControl) so this only ever tracks the grammar panel now.
  const [openGrammarIndex, setOpenGrammarIndex] = useState<number | null>(null);
  const [prInput, setPrInput] = useState("");
  const [prError, setPrError] = useState(false);
  const [loadingMoreHistory, setLoadingMoreHistory] = useState(false);
  // Once the learner scrolls away from the latest turn, keep an explicit
  // way back instead of making them drag through a long transcript.
  const [showScrollToLatest, setShowScrollToLatest] = useState(false);

  const clientRef = useRef<BuddyClient | null>(null);
  const recorderRef = useRef<PCMRecorder | null>(null);
  const audioRef = useRef<HTMLAudioElement | null>(null);
  const menuRef = useRef<HTMLDivElement>(null);
  const studyRef = useRef<HTMLDivElement>(null);
  // The scrollable message list (see the .convo <main> below) — read/written
  // directly by the scroll-to-bottom and load-older-history effects, since
  // scroll position isn't state React should own a re-render for.
  const convoRef = useRef<HTMLElement | null>(null);
  // Whether the view should follow new content added at the end of msgs
  // (a fresh room entry, or a live message arriving while already at/near
  // the bottom) — false once the learner has scrolled up to read older
  // turns, so a background poll landing a translation doesn't yank them back
  // down. Kept as a ref, not state: it's written on every scroll event and
  // must never itself trigger a render.
  const stickToBottomRef = useRef(true);
  // A physical marker immediately after the newest transcript content. The
  // scroll-distance calculation is the fast path, but iOS Safari can briefly
  // report stale scroll dimensions while its browser chrome changes. Watching
  // this marker makes the visible transcript end authoritative for dismissing
  // the "latest message" toast.
  const latestMessageAnchorRef = useRef<HTMLDivElement | null>(null);
  // Set just before loadOlderTurns prepends older turns to msgs, consumed
  // once by the scroll-position effect below to hold the visual scroll
  // position steady across the height added above (rather than the natural
  // "still scrolled 80px from a now-relocated top" jump). Null the rest of
  // the time, including while stickToBottomRef governs a normal append.
  const prependAdjustRef = useRef<{ scrollHeight: number; scrollTop: number } | null>(null);
  // Whether older turns exist beyond what's currently loaded into msgs (see
  // enterChat's/loadOlderTurns' hasMore) — scrolling to the top of .convo
  // while this is true triggers loadOlderTurns. Only read inside the scroll
  // handler, never rendered, so it's a ref rather than state — setting it
  // shouldn't force a re-render on every page load.
  const hasMoreHistoryRef = useRef(false);
  // True once the open room's history entry sits on top of a "list" entry
  // this app itself pushed — set on every list->chat transition (enterChat's
  // push, or popping forward into a room) and cleared back on the list, so
  // backToList's button can reuse that entry (goBack) and stay in sync with
  // what browser back/swipe-back would do, instead of pushing a redundant one.
  const hasPushedRoomEntryRef = useRef(false);

  // Clears UI/media that only belongs to the room being left. In particular,
  // an unconfirmed voice transcript must never be tagged as a voice message
  // in the next room, and a microphone recording must not continue invisibly
  // behind the room list.
  const resetTransientChatState = useCallback(() => {
    const recorder = recorderRef.current;
    if (recorder?.isRecording) {
      // stop() also releases the MediaStream tracks. Its recognized audio is
      // intentionally discarded because the learner left the room.
      void Promise.resolve(recorder.stop()).catch((err) => console.error("mic cleanup:", err));
    }
    setMic(false);
    setTranscribing(false);
    setVoiceDraft(false);
    voiceDraftAutoTextRef.current = null;
    setOpenGrammarIndex(null);
    setChatError(null);
    setShowScrollToLatest(false);

    const audio = audioRef.current;
    if (audio) {
      audio.pause();
      audio.removeAttribute("src");
    }
  }, []);

  // Points the shared <audio> element at url and plays it — generated and
  // cached server-side per (session, turn, role) (see lib/sessions.ts's
  // messageAudioURL), so this is just "src + play(), let the browser handle
  // buffering", the same shape as ArticleQuiz.tsx's handleRead. Used by both
  // playMessage (a tap on a message's own 🔊 button) and onEvent's
  // assistant_done auto-read — has no reactive dependencies (audioRef is a
  // stable ref), so it's safe for the latter's permanently-stable closure to
  // call.
  const playAudio = useCallback((url: string, rate: number) => {
    const el = audioRef.current;
    if (!el) return;
    // Must run synchronously with play() — see requestAmbientAudioSession's
    // doc comment.
    requestAmbientAudioSession();
    setChatError(null);
    el.playbackRate = rate;
    el.src = url;
    el.play().catch((err) => {
      console.error("tts:", err);
      setChatError("음성을 재생하지 못했어요. 잠시 후 다시 시도해주세요.");
    });
  }, []);

  const onEvent = useCallback((e: ServerEvent) => {
    // A permanently-ended room closes its WS connection (see enterChat), but
    // that close is asynchronous — anything already in flight (e.g. a
    // background job's result) must still be ignored rather than reviving a
    // room that's supposed to be a frozen, read-only snapshot.
    if (endedRef.current) return;
    switch (e.type) {
      case "ready":
        // Upgrades a brand-new room's placeholder history entry to carry its
        // real id, once the server has minted one — only when we're still on
        // that pending entry, so reconnecting to an already-known room (which
        // also emits "ready") never touches history.
        if (e.session) {
          setActiveSessionId(e.session);
          const current = currentRoomHistoryState();
          if (current.view === "chat" && current.id == null) {
            replaceRoomState({ view: "chat", id: e.session });
          }
          // Flags this brand-new room as instant right away, before the
          // learner has even said anything — a plain fire-and-forget call
          // (markInstant is an idempotent upsert, so a reconnect re-sending
          // "ready" for the same room is harmless). Reconnecting to an
          // *existing* room never has quickModeRef set (it's only ever true
          // for the enterChat(undefined, { quick: true }) path that opened
          // this room in the first place), so this never mis-fires for a
          // normal room.
          if (quickModeRef.current) void markInstant(e.session);
        }
        break;
      case "pending_transcript": {
        // A still-editable STT guess for a spoken utterance — not yet part
        // of the conversation. Drop it into the composer for the learner to
        // review/edit and explicitly send (see submitText), instead of
        // committing it automatically: STT can hallucinate words the
        // learner never actually said, so nothing here spends a
        // reply/correction call until they confirm it. Only apply a later
        // (Judge-upgraded) guess if the learner hasn't started editing the
        // draft themselves.
        const incoming = e.text ?? "";
        setTranscribing(false);
        setChatError(null);
        // Captured synchronously, before the ref is updated below — setText's
        // updater runs later (deferred/batched), so it must not read the
        // ref's live value at that point, only what it was when this event
        // arrived.
        const wasUntouched = voiceDraftAutoTextRef.current === null;
        const priorAutoText = voiceDraftAutoTextRef.current;
        voiceDraftAutoTextRef.current = incoming;
        setText((prev) => (wasUntouched || prev === priorAutoText ? incoming : prev));
        setVoiceDraft(true);
        break;
      }
      case "final_transcript":
        setMsgs((m) => [
          ...m,
          {
            turn: e.turn,
            role: "user",
            text: e.text ?? "",
            source: e.source,
            timestamp: Math.floor(Date.now() / 1000),
          },
        ]);
        patchTurn(e.turn, { correctionPending: true, correctionFailed: false, userTranslationPending: true });
        break;
      case "refined_transcript":
        setMsgs((m) =>
          m.map((x) =>
            x.turn === e.turn && x.role === "user"
              ? { ...x, text: e.text ?? x.text, refined: true, source: e.source ?? x.source }
              : x,
          ),
        );
        break;
      case "assistant_delta":
        setAwaitingReply(false);
        setMsgs((m) => upsertAssistant(m, e.turn, (prev) => prev + (e.text ?? "")));
        break;
      case "assistant_done":
        setAwaitingReply(false);
        setMsgs((m) => upsertAssistant(m, e.turn, () => e.text ?? ""));
        patchTurn(e.turn, { assistantTranslationPending: true });
        if (e.text && autoReadAloudRef.current && activeSessionIdRef.current) {
          playAudio(messageAudioURL(activeSessionIdRef.current, e.turn, "assistant"), playbackRateRef.current);
        }
        // Turn 0 is the room's own opening greeting (see protocol.ts), not
        // the learner's sentence — only the first *real* reply is what a
        // quick-mode room is waiting to wrap up after.
        if (quickModeRef.current && e.turn >= 1) {
          setQuickWatchTurn((prev) => (prev === null ? e.turn : prev));
        }
        break;
      case "correction":
        // A correction first arrives as a readable Chat preview, then as a
        // terminal Judge result. Keep the preview available while the later
        // stages run, and mark the final result unread only when it actually
        // changed (or when there was no usable preview at all).
        setTurns((prev) => {
          const prior = prev[e.turn] ?? {};
          const changed = e.changed ?? (!!e.correction && (
            !prior.correction || JSON.stringify(prior.correction) !== JSON.stringify(e.correction)
          ));
          return {
            ...prev,
            [e.turn]: {
              ...prior,
              ...(e.correction ? { correction: e.correction } : {}),
              ...(e.correction?.translation ? { userTranslation: e.correction.translation } : {}),
              correctionPending: !e.final && !e.failed,
              correctionFailed: !!e.failed,
              correctionUnread: e.final && !e.failed
                ? (changed || !!prior.correctionUnread)
                : prior.correctionUnread,
              userTranslationPending: !e.correction?.translation && !e.final && !e.failed,
            },
          };
        });
        break;
      case "user_translation":
        patchTurn(e.turn, { userTranslation: e.text ?? "" });
        break;
      case "assistant_translation":
        patchTurn(e.turn, { assistantTranslation: e.text ?? "", assistantTranslationPending: false });
        break;
      case "error":
        setAwaitingReply(false);
        setTranscribing(false);
        console.error("server error:", e.text);
        setChatError(
          e.text?.startsWith("stt:")
            ? "음성을 인식하지 못했어요. 다시 녹음하거나 직접 입력해주세요."
            : "요청을 처리하지 못했어요. 잠시 후 다시 시도해주세요.",
        );
        break;
    }
  }, [patchTurn]);

  useEffect(() => {
    if (clientRef.current) return; // guard StrictMode double-invoke
    // Created eagerly so mic/voice setup and the WS client are ready the
    // moment a room is opened, but connect() is NOT called here — the home
    // screen shows the room list first, not a silently reconnected chat.
    clientRef.current = new BuddyClient(onEvent, setStatus);
    recorderRef.current = new PCMRecorder();
    return () => {
      clientRef.current?.close();
      clientRef.current = null;
    };
  }, [onEvent]);

  const refreshSessions = useCallback(async () => {
    setSessionsLoading(true);
    setSessionsLoadError(false);
    const next = await fetchSessions();
    if (next === null) {
      // Preserve any list already on screen during a failed background
      // refresh; only the initial load has no rows to keep.
      setSessionsLoadError(true);
    } else {
      setSessions(next);
    }
    setSessionsLoading(false);
  }, []);

  useEffect(() => {
    void refreshSessions();
  }, [refreshSessions]);

  // Keeps the "정리 중" badge (see the room-list render below) honest while
  // sitting on the list: a room's study-summary job (see
  // asyncjob.KindStudySummary) keeps running in the background regardless of
  // whether the learner is watching, so without this the badge would only
  // ever update on the next full list load. Re-runs whenever `sessions`
  // changes (including from this refresh itself), and naturally stops
  // scheduling once nothing is left pending — no separate cancellation
  // token needed, since the effect's own cleanup clears a still-pending
  // timer if the learner leaves the list before it fires.
  useEffect(() => {
    if (view !== "list") return;
    if (!sessions.some((s) => s.studySummaryStatus === "pending")) return;
    const t = setTimeout(() => void refreshSessions(), 4000);
    return () => clearTimeout(t);
  }, [view, sessions, refreshSessions]);

  // Deletes a chat room from the list without opening it. The server also
  // cascades to any recordings archived under that room (see
  // httpserver.sessionDeleteHandler), so this is the one action that clears
  // both the transcript and its audio.
  const handleDeleteSession = useCallback(async (id: string) => {
    if (!window.confirm("이 대화를 삭제할까요? 저장된 녹음도 함께 삭제됩니다.")) return;
    setListActionError(null);
    setDeletingSessionId(id);
    const deleted = await deleteSession(id);
    setDeletingSessionId(null);
    if (deleted) {
      setSessions((list) => list.filter((session) => session.id !== id));
    } else {
      setListActionError("대화를 삭제하지 못했어요. 연결을 확인한 뒤 다시 시도해주세요.");
    }
  }, []);

  // Poll scaffolding for pollMissingFeedback/pollStudySummary/pollQuizStatus
  // below (see usePollScaffold's doc comment). pollTokenRef identifies the
  // most recent poll chain so a slow fetch that resolves after the learner
  // already left the room, or opened a different one, doesn't apply its
  // (now stale) result to the wrong room's state.
  const { tokenRef: pollTokenRef, schedulePoll } = usePollScaffold();

  // Polls a room's transcript for translations, grammar corrections, and
  // assistant replies the server is still working on in the background —
  // backfilling a translation (see internal/backfill), still running
  // correct() for a turn that was in flight when the learner left the room,
  // or (see internal/asyncjob) a reply job that outlived the connection
  // that started it — either because this replica died before finishing it,
  // or another replica ended up running it instead. Opening a room only
  // fetches its transcript once, and none of these background paths has a
  // push channel to tell an already-open client "it's ready now", so
  // without this a turn still missing its translation/correction/reply
  // would just show a spinner that never resolves until the learner leaves
  // and reopens the room — which was exactly the bug: the grammar hourglass
  // would vanish on reopen instead of resuming, since nothing repopulated
  // pendingCorrections for a hydrated turn whose correction hadn't landed
  // yet. Stops once nothing is missing anymore or maxAttempts is reached; a
  // fresh call to enterChat/backToList invalidates `token` so a slow,
  // late-arriving response never overwrites a different room's state.
  const pollMissingFeedback = useCallback((sessionId: string, token: object) => {
    const maxAttempts = 20;
    const intervalMs = 4000;
    let attempt = 0;
    const tick = async () => {
      if (pollTokenRef.current !== token) return; // left this room, or opened another
      attempt++;
      // limit: 0 asks for the whole transcript, not just the latest page —
      // a pending correction/translation/reply can sit on any turn, not
      // just ones on the page currently loaded into msgs.
      const detail = await fetchSessionDetail(sessionId, { limit: 0 });
      if (pollTokenRef.current !== token || !detail) return;
      let stillMissing = false;
      const patches: Record<number, Partial<TurnMeta>> = {};
      for (const t of detail.turns) {
        if (t.role === "assistant" && !t.text) {
          // A reply job still pending/processing (see
          // store.Turn.ReplyStatus) has no text yet — nothing to hydrate
          // into msgs this tick, just keep polling. A turn with no
          // replyStatus at all and no text is a pre-existing, permanently
          // empty row (shouldn't normally happen) — not worth polling
          // forever for.
          if (t.replyStatus === "pending" || t.replyStatus === "processing") stillMissing = true;
          continue;
        }
        if (t.role === "assistant" && t.replyStatus === "done") {
          // The reply finished after this connection either lost the
          // fast-path race or never got to run it at all (see
          // transport.NewReplyHook) — materialize it as a whole bubble
          // (no token-by-token typing, unlike the live WS path) and clear
          // the typing indicator a pending-reply hydration turned on.
          setMsgs((m) => upsertAssistant(m, t.turn, () => t.text));
          setAwaitingReply(false);
        }
        const savedTranslation = t.translation || (t.role === "user" ? t.correction?.translation : undefined);
        if (savedTranslation) {
          patches[t.turn] = {
            ...patches[t.turn],
            ...(t.role === "user"
              ? { userTranslation: savedTranslation }
              : { assistantTranslation: savedTranslation }),
          };
        } else if (t.text) {
          stillMissing = true;
        }
        if (t.role === "user" && t.text) {
          if (t.correction) {
            const refining = t.correctionStage === "chat" || t.correctionStatus === "pending" || t.correctionStatus === "processing";
            patches[t.turn] = {
              ...patches[t.turn],
              correction: t.correction,
              correctionPending: refining,
              correctionFailed: false,
              correctionUnread: !!t.correctionUnread,
            };
            if (refining) stillMissing = true;
          } else if (t.correctionStatus === "failed") {
            // The job errored, but the reaper (see internal/asyncjob) still
            // retries it from scratch on its own — keep polling in case a
            // later attempt succeeds, while showing the failed state in the
            // meantime instead of a spinner that looks like it's still
            // working on the first attempt.
            patches[t.turn] = { ...patches[t.turn], correctionPending: false, correctionFailed: true };
            stillMissing = true;
          } else {
            // No correctionStatus at all (saved before correction-job
            // tracking existed, or the no-Redis inline path) — no live job
            // was ever reserved for the reaper above to retry. This
            // fetchSessionDetail call is itself what queues it for
            // server-side backfill (see httpserver.sessionDetailHandler /
            // internal/backfill.CorrectionWorker), so keep polling the same
            // way as the "failed" branch above.
            stillMissing = true;
          }
        }
      }
      if (Object.keys(patches).length > 0) patchTurns(patches);
      if (stillMissing && attempt < maxAttempts) schedulePoll(tick, intervalMs);
    };
    schedulePoll(tick, intervalMs);
  }, [patchTurns, schedulePoll]);

  // Opening the feedback panel is the point where "unread" becomes read.
  // Persist that acknowledgement on the server, then clear local state only
  // after it succeeds so a network error cannot silently lose the reminder.
  const markingCorrectionReadRef = useRef(new Set<string>());
  useEffect(() => {
    if (openGrammarIndex === null || !activeSessionId) return;
    const msg = msgs[openGrammarIndex];
    if (!msg || msg.role !== "user" || !turns[msg.turn]?.correctionUnread) return;
    const key = `${activeSessionId}:${msg.turn}`;
    if (markingCorrectionReadRef.current.has(key)) return;
    markingCorrectionReadRef.current.add(key);
    void markCorrectionRead(activeSessionId, msg.turn).then((ok) => {
      markingCorrectionReadRef.current.delete(key);
      if (!ok) return;
      patchTurn(msg.turn, { correctionUnread: false });
      setSessions((prev) => prev.map((s) => s.id === activeSessionId
        ? { ...s, unreadCorrections: Math.max(0, (s.unreadCorrections ?? 0) - 1) }
        : s));
    });
  }, [activeSessionId, msgs, openGrammarIndex, patchTurn, turns]);

  // Shared shape behind pollStudySummary/pollQuizStatus below: poll
  // fetchSessionDetail until a session-level background job's status field
  // reaches "done" (or maxAttempts runs out), applying whatever value came
  // back on every tick regardless of status so a "pending"/"failed" mid-poll
  // state renders too, not just the final one.
  const pollSessionField = useCallback(
    <T,>(
      sessionId: string,
      token: object,
      maxAttempts: number,
      getField: (s: SessionDetail["session"]) => { status: "pending" | "done" | "failed"; value: T },
      apply: (status: "pending" | "done" | "failed", value: T) => void,
    ) => {
      const intervalMs = 4000;
      let attempt = 0;
      const tick = async () => {
        if (pollTokenRef.current !== token) return; // left this room, or opened another
        attempt++;
        const detail = await fetchSessionDetail(sessionId, { limit: 0 });
        if (pollTokenRef.current !== token || !detail) return;
        const { status, value } = getField(detail.session);
        apply(status, value);
        if (status !== "done" && attempt < maxAttempts) schedulePoll(tick, intervalMs);
      };
      schedulePoll(tick, intervalMs);
    },
    [pollTokenRef, schedulePoll],
  );

  // Polls an ended room's background study-summary job (see
  // asyncjob.KindStudySummary) until it lands — the same "no push channel to
  // an already-open client" gap pollMissingFeedback fills for turn-level
  // jobs, just for the session-level wrap-up instead. Reuses pollTokenRef
  // (set by enterChat, cleared by resetToListView) so leaving the room stops
  // this the same way it stops pollMissingFeedback. "failed" keeps polling
  // rather than giving up: the reaper (see internal/asyncjob) retries a
  // failed attempt on its own, so a later attempt may still land.
  const pollStudySummary = useCallback(
    (sessionId: string, token: object) =>
      pollSessionField(
        sessionId,
        token,
        30,
        (s) => ({ status: s.studySummaryStatus || "done", value: s.studySummary ?? [] }),
        (status, value) => {
          setEndedSummaryStatus(status);
          setEndedSummary(value);
        },
      ),
    [pollSessionField],
  );

  // Mirrors pollStudySummary exactly, but for the quiz pre-generation job
  // (see asyncjob.KindStudyQuiz) — a separate poller, not folded into the
  // one above, since the two background jobs run independently and can land
  // at different times.
  const pollQuizStatus = useCallback(
    (sessionId: string, token: object) =>
      pollSessionField(
        sessionId,
        token,
        30,
        (s) => ({ status: s.quizStatus || "done", value: s.quiz ?? [] }),
        (status, value) => {
          setEndedQuizStatus(status);
          setEndedQuiz(value);
        },
      ),
    [pollSessionField],
  );

  // Opens a room and enters chat view. sessionId omitted starts a brand-new
  // room (server mints the ID, delivered on the "ready" event); given an
  // existing ID, this hydrates the visible transcript from its persisted
  // history first, since reconnecting the WS alone only seeds LLM context,
  // it doesn't replay old chat bubbles.
  const enterChat = useCallback(
    async (sessionId?: string, opts?: { push?: boolean; quick?: boolean }) => {
      resetTransientChatState();
      resetTurnState();
      resetEndedState();
      resetQuickState(!!opts?.quick);
      setMsgs([]);
      setListActionError(null);
      setOpeningSessionId(sessionId ?? null);
      // A fresh room entry always starts stuck to the bottom (the most
      // recent turns, loaded below) with no older page pending — cleared
      // again if this turns out to be a brand-new room with nothing to page
      // through at all.
      stickToBottomRef.current = true;
      prependAdjustRef.current = null;
      hasMoreHistoryRef.current = false;
      setLoadingMoreHistory(false);
      const token = {};
      pollTokenRef.current = token;
      if (opts?.push ?? true) {
        // Gives this room its own history entry on top of the list's, so
        // browser back/swipe-back leaves the room instead of the whole app
        // (see backToList and the popstate handler in the mount effect).
        pushRoomState({ view: "chat", id: sessionId ?? null });
        hasPushedRoomEntryRef.current = true;
      }
      setActiveSessionId(sessionId ?? null);
      // Restore whatever draft this room had cached (see draftCache.ts) —
      // otherwise a leftover draft from the previous room would silently
      // carry over into this one, or a real draft would look lost.
      setText(sessionId ? loadDraft(sessionId) : "");
      if (sessionId) {
        // Fire the WS handshake alongside the transcript fetch — they're
        // independent round trips — instead of waiting for the fetch first.
        clientRef.current?.connect(sessionId);
        // Only the most recent HISTORY_PAGE_SIZE turns load up front — a
        // long-running room's whole history would otherwise ship (and
        // render) on every visit; loadOlderTurns fetches the rest as the
        // learner scrolls up. See PAGE_SIZE and defaultSessionPageLimit
        // (apps/server/internal/httpserver/server.go).
        const detail = await fetchSessionDetail(sessionId, { limit: HISTORY_PAGE_SIZE });
        if (!detail) {
          clientRef.current?.close(); // fetch failed (e.g. deleted elsewhere) — stay on the list
          replaceRoomState({ view: "list" });
          hasPushedRoomEntryRef.current = false;
          setActiveSessionId(null);
          setOpeningSessionId(null);
          setListActionError("대화를 열지 못했어요. 연결을 확인한 뒤 다시 시도해주세요.");
          setView("list");
          return;
        }
        hasMoreHistoryRef.current = detail.hasMore;
        setMsgs(turnsToMsgs(detail.turns));
        if (detail.session.ended) {
          // Read-only from here on: no live connection needed, so drop the
          // one fired eagerly above — the composer disables itself once
          // `ended` renders (see the footer below), and onEvent (guarded by
          // endedRef) ignores anything that arrives anyway.
          setEnded(true);
          endedRef.current = true;
          setEndedSummary(detail.session.studySummary ?? []);
          const summaryStatus = detail.session.studySummaryStatus || "done";
          setEndedSummaryStatus(summaryStatus);
          setEndedQuiz(detail.session.quiz ?? []);
          const quizStatus = detail.session.quizStatus || "done";
          setEndedQuizStatus(quizStatus);
          setEndedQuizCompleted(detail.session.quizCompleted ?? false);
          clientRef.current?.close();
          // The wrap-up is still generating (or the last attempt failed —
          // see pollStudySummary's doc comment) — keep checking until it
          // lands, since nothing pushes it to an already-open client.
          if (summaryStatus !== "done") pollStudySummary(sessionId, token);
          // Same reasoning, for the quiz pre-generation job running
          // independently alongside it — see pollQuizStatus.
          if (quizStatus !== "done") pollQuizStatus(sessionId, token);
        }
        // Whether this room saw activity recently enough that a user turn
        // with no correctionStatus at all is plausibly still in flight
        // (rather than a session from before correction-job tracking
        // existed) — only needed for that untracked case; a turn that DOES
        // have a correctionStatus (pending/failed) tells us its actual state
        // directly, no guessing required (see store.Turn.CorrectionStatus).
        const recentlyActive = Date.now() / 1000 - detail.session.updatedAt < 120;
        const hydrated: Record<number, TurnMeta> = {};
        let anyPending = false;
        for (const t of detail.turns) {
          if (isPendingPlaceholder(t)) {
            // Reply still in flight — show the same typing indicator a
            // brand-new room's opening line gets, and poll until
            // pollMissingFeedback sees it complete.
            if (t.replyStatus === "pending" || t.replyStatus === "processing") {
              setAwaitingReply(true);
              anyPending = true;
            }
            continue;
          }
          const { meta, pending } = hydrateTurnMeta(t, recentlyActive);
          if (pending) anyPending = true;
          // A user turn and its paired assistant reply share the same turn
          // number, so merge rather than overwrite.
          hydrated[t.turn] = { ...hydrated[t.turn], ...meta };
        }
        setTurns(hydrated);
        if (anyPending) pollMissingFeedback(sessionId, token);
      } else {
        setMsgs([]);
        clientRef.current?.connect(undefined);
        // A brand-new room gets an opening line from the server before the
        // learner says anything (see pipeline.StartConversation) — show the
        // typing indicator right away instead of a bare empty screen.
        setAwaitingReply(true);
      }
      setMenuOpen(false);
      setOpeningSessionId(null);
      setView("chat");
    },
    [
      resetTransientChatState,
      resetTurnState,
      resetEndedState,
      resetQuickState,
      pollMissingFeedback,
      pollStudySummary,
      pollQuizStatus,
    ],
  );

  // Fetches the page of turns older than whatever's currently loaded —
  // triggered by scrolling .convo to (near) its top, see handleConvoScroll.
  // Prepends onto msgs/turns rather than replacing them, and records the
  // pre-prepend scroll metrics in prependAdjustRef so the scroll-position
  // effect below can hold the visible content steady instead of letting the
  // added height above shove the viewport down.
  const loadOlderTurns = useCallback(async () => {
    const oldestTurn = msgs[0]?.turn;
    if (!activeSessionId || oldestTurn == null || loadingMoreHistory) return;
    setLoadingMoreHistory(true);
    const el = convoRef.current;
    if (el) prependAdjustRef.current = { scrollHeight: el.scrollHeight, scrollTop: el.scrollTop };
    const detail = await fetchSessionDetail(activeSessionId, {
      before: oldestTurn,
      limit: HISTORY_PAGE_SIZE,
    });
    setLoadingMoreHistory(false);
    if (!detail) {
      prependAdjustRef.current = null; // fetch failed — nothing to hold position for
      setChatError("이전 대화를 불러오지 못했어요. 위로 스크롤하면 다시 시도할게요.");
      return;
    }
    hasMoreHistoryRef.current = detail.hasMore;
    if (detail.turns.length === 0) return;

    setMsgs((m) => [...turnsToMsgs(detail.turns), ...m]);

    const hydrated: Record<number, TurnMeta> = {};
    for (const t of detail.turns) {
      if (isPendingPlaceholder(t)) continue; // stale pending placeholder from way back — nothing to show
      const { meta } = hydrateTurnMeta(t, false);
      hydrated[t.turn] = { ...hydrated[t.turn], ...meta };
    }
    setTurns((prev) => ({ ...hydrated, ...prev }));
  }, [activeSessionId, msgs, loadingMoreHistory]);

  // Shared by backToList and the popstate handler below — actually leaving
  // the room, as opposed to deciding how the browser's history entry should
  // reflect that.
  const resetToListView = useCallback(() => {
    pollTokenRef.current = null;
    clientRef.current?.close();
    resetTransientChatState();
    setMsgs([]);
    resetTurnState();
    resetEndedState();
    resetQuickState(false);
    setMenuOpen(false);
    setView("list");
    setActiveSessionId(null);
    setOpeningSessionId(null);
    hasMoreHistoryRef.current = false;
    setLoadingMoreHistory(false);
    stickToBottomRef.current = true;
    hasPushedRoomEntryRef.current = false;
    void refreshSessions();
  }, [refreshSessions, resetTransientChatState, resetTurnState, resetEndedState, resetQuickState]);

  // Holds the .convo scroll position steady when new content is added:
  // pinned to the bottom for a fresh room entry or a live message arriving
  // while already there (stickToBottomRef), or held at the same visual spot
  // when loadOlderTurns prepends older turns above what's currently in view
  // (prependAdjustRef, consumed once per prepend).
  useLayoutEffect(() => {
    const el = convoRef.current;
    if (!el) return;
    if (prependAdjustRef.current) {
      const { scrollHeight, scrollTop } = prependAdjustRef.current;
      el.scrollTop = scrollTop + (el.scrollHeight - scrollHeight);
      prependAdjustRef.current = null;
      return;
    }
    if (stickToBottomRef.current) {
      el.scrollTop = el.scrollHeight;
      setShowScrollToLatest(false);
    }
  }, [msgs]);

  // Tracks whether the learner is at/near the bottom (stickToBottomRef, so
  // later message arrivals know whether to auto-scroll) and, near the top,
  // triggers loadOlderTurns — the "끊어서 스크롤을 올리면 갱신" behavior.
  const handleConvoScroll = useCallback(() => {
    const el = convoRef.current;
    if (!el) return;
    const distanceFromBottom = el.scrollHeight - el.scrollTop - el.clientHeight;
    const anchor = latestMessageAnchorRef.current;
    const rootRect = el.getBoundingClientRect();
    const anchorRect = anchor?.getBoundingClientRect();
    // WebKit can keep the scroll metrics stale while Safari's dynamic browser
    // chrome settles. In that case the end marker's rendered position is the
    // reliable signal: once it reaches the scroll viewport (or its bottom
    // threshold), the learner is already looking at the latest turn. Keep the
    // metric path as a fallback for non-layout environments and browsers.
    const markerAtLatest = !!anchorRect
      && rootRect.height > 0
      && anchorRect.top <= rootRect.bottom + SCROLL_EDGE_THRESHOLD;
    const atLatest = markerAtLatest || distanceFromBottom < SCROLL_EDGE_THRESHOLD;
    stickToBottomRef.current = atLatest;
    setShowScrollToLatest(!atLatest);
    if (el.scrollTop < SCROLL_EDGE_THRESHOLD && hasMoreHistoryRef.current && !loadingMoreHistory) {
      void loadOlderTurns();
    }
  }, [loadingMoreHistory, loadOlderTurns]);

  // On mobile Safari, dynamic browser controls can leave scrollHeight,
  // scrollTop, and clientHeight temporarily out of sync even after the final
  // row is visibly on screen. A bottom marker is measured against the actual
  // scroll viewport, so it reliably clears a stale jump toast. The scroll
  // handler above remains the fallback for browsers without this API.
  useEffect(() => {
    const root = convoRef.current;
    const target = latestMessageAnchorRef.current;
    if (!root || !target || typeof IntersectionObserver === "undefined") return;

    const observer = new IntersectionObserver(
      ([entry]) => {
        if (!entry?.isIntersecting) return;
        stickToBottomRef.current = true;
        setShowScrollToLatest(false);
      },
      { root, rootMargin: `0px 0px ${SCROLL_EDGE_THRESHOLD}px 0px` },
    );
    observer.observe(target);
    return () => observer.disconnect();
  }, [view]);

  const scrollToLatest = useCallback(() => {
    const el = convoRef.current;
    if (!el) return;
    stickToBottomRef.current = true;
    el.scrollTop = el.scrollHeight;
    setShowScrollToLatest(false);
  }, []);

  const backToList = useCallback(() => {
    if (hasPushedRoomEntryRef.current) {
      // Reuse the list entry this room's own history entry was pushed on top
      // of, so this button and an actual browser back/swipe do the same
      // thing — the popstate handler in the mount effect does the reset.
      goBack();
      return;
    }
    // No list entry to pop back to (e.g. this room was restored on mount
    // from a refreshed URL) — swap the current entry instead of leaving one
    // more room hash for the next back/swipe to trip over.
    replaceRoomState({ view: "list" });
    resetToListView();
  }, [resetToListView]);

  // Confirms "end this conversation": freezes the room read-only immediately
  // server-side (see httpserver.sessionEndHandler), which kicks off the
  // study wrap-up as a background job from there — not something this call
  // waits on — then leaves the room the same way the plain back button
  // does. Fire-and-forget on the network call — leaving feels instant, and
  // the freeze itself is a fast plain UPDATE regardless.
  const endConversation = useCallback(() => {
    if (activeSessionId) void endSession(activeSessionId);
    backToList();
  }, [activeSessionId, backToList]);

  // Forces a stuck "no issues found" wrap-up to regenerate — see
  // httpserver.sessionRestudyHandler, which only accepts this while
  // studySummaryStatus is "done" with an empty summary (the same state
  // EndConversationControl gates showing its "다시 확인하기" button on): an
  // occasional case where GenerateStudySummary's LLM call returned a
  // valid-but-empty result despite real issues in the transcript, with no
  // automatic way back to pending. Switches straight to the "정리 중" view
  // and reuses pollStudySummary the same way reopening a still-generating
  // room does, since the background job runs the identical path either way.
  const restudyConversation = useCallback(async () => {
    if (!activeSessionId) return;
    const token = pollTokenRef.current;
    setEndedSummaryStatus("pending");
    const ok = await restudySession(activeSessionId);
    if (!ok) {
      setEndedSummaryStatus("failed");
      return;
    }
    if (token) pollStudySummary(activeSessionId, token);
  }, [activeSessionId, pollStudySummary]);

  // Regenerates an ended session's practice quiz from scratch on demand —
  // see httpserver.sessionQuizResetHandler, the "퀴즈 다시 만들기" button in
  // EndConversationControl. Unlike restudyConversation, this isn't limited
  // to a stuck-empty result: a learner can ask for a fresh set of questions
  // even when the current quiz already has real content (e.g. one generated
  // before answerMeaning/acceptableAnswers existed). Clears the local quiz
  // state immediately so the panel shows "준비하는 중" rather than the stale
  // quiz while the background job regenerates it, and reuses pollQuizStatus
  // the same way reopening a still-generating room does.
  const resetConversationQuiz = useCallback(async () => {
    if (!activeSessionId) return;
    const token = pollTokenRef.current;
    setEndedQuiz([]);
    setEndedQuizStatus("pending");
    setEndedQuizCompleted(false);
    const ok = await resetQuiz(activeSessionId);
    if (!ok) {
      setEndedQuizStatus("failed");
      return;
    }
    if (token) pollQuizStatus(activeSessionId, token);
  }, [activeSessionId, pollQuizStatus]);

  // Reflects a just-completed quiz (see markQuizCompleted, called from
  // QuizPanel/EndConversationControl) in this room's own state immediately —
  // the room list itself picks up the persisted flag separately, the next
  // time it refreshes (see resetToListView), so this is just so the panel
  // doesn't show a stale "퀴즈 풀기"/"내가 읽었음" button for the rest of this
  // visit.
  const handleQuizCompleted = useCallback(() => {
    setEndedQuizCompleted(true);
  }, []);

  // Wraps up an "인스턴트 대화" room on its own, the moment the one exchange it
  // was opened for has its grammar correction back — same freeze + background
  // wrap-up as a learner-confirmed endConversation, just triggered
  // automatically instead of behind the 🎓 button, and without leaving the
  // room (so the reply and its correction stay visible right where the
  // learner is, instead of bouncing back to the list). Gates on
  // correctionPending specifically, not the translation-pending flags too:
  // correct() is guaranteed to emit exactly one "correction" event per turn,
  // success or failure (see pipeline.correct's doc comment), but
  // translateAssistant can fail silently with no event at all — gating on it
  // here would leave a transient translation outage wedging quick mode open
  // forever. Once frozen, pollMissingFeedback (the same poll a reopened
  // ended room already relies on) picks up any translation that was still
  // mid-flight at that instant, since onEvent drops the live event for it
  // the moment endedRef flips.
  useEffect(() => {
    if (!quickMode || quickWatchTurn === null || ended || !activeSessionId) return;
    if (quickEndingRef.current) return;
    const meta = turns[quickWatchTurn];
    if (!meta || meta.correctionPending) return;
    quickEndingRef.current = true;
    const token = pollTokenRef.current;
    void endSession(activeSessionId).then((ok) => {
      if (!ok) {
        quickEndingRef.current = false; // let a later retry (e.g. another turns update) try again
        return;
      }
      clientRef.current?.close();
      setEnded(true);
      endedRef.current = true;
      setEndedSummaryStatus("pending");
      setEndedQuizStatus("pending");
      if (token) {
        pollStudySummary(activeSessionId, token);
        pollQuizStatus(activeSessionId, token);
        pollMissingFeedback(activeSessionId, token);
      }
    });
  }, [quickMode, quickWatchTurn, ended, activeSessionId, turns, pollStudySummary, pollQuizStatus, pollMissingFeedback]);

  // Restores an open room from the URL on a fresh load (e.g. a refresh), and
  // keeps the view in sync with browser back/forward (incl. swipe) — neither
  // enterChat nor backToList touch React state directly for that path, since
  // the browser has already changed the URL by the time popstate fires.
  useEffect(() => {
    const initial = parseRoomHash(window.location.hash);
    if (initial.view === "chat" && initial.id) {
      replaceRoomState(initial);
      void enterChat(initial.id, { push: false });
    } else {
      replaceRoomState({ view: "list" });
    }

    return onRoomPopState((state) => {
      if (state.view === "chat" && state.id) {
        hasPushedRoomEntryRef.current = true;
        void enterChat(state.id, { push: false });
      } else {
        resetToListView();
      }
    });
  }, [enterChat, resetToListView]);

  // A dropped connection can never deliver the reply the learner is waiting
  // on, so don't leave the typing indicator spinning forever.
  useEffect(() => {
    if (status === "closed" || status === "error") setAwaitingReply(false);
  }, [status]);

  // Clears an unsent voice draft from the composer — called when a new
  // recording starts (see toggleMic) or the learner explicitly discards one.
  const discardVoiceDraft = useCallback(() => {
    setText("");
    setVoiceDraft(false);
    voiceDraftAutoTextRef.current = null;
  }, []);

  const toggleMic = useCallback(async () => {
    const rec = recorderRef.current;
    if (!rec || transcribing) return;
    if (rec.isRecording) {
      try {
        const pcm = await rec.stop();
        setMic(false);
        if (pcm.length > 0) {
          clientRef.current?.sendAudio(pcm);
          // No reply is coming until the learner reviews and sends the
          // resulting draft (see the "pending_transcript" case in onEvent) —
          // just show that STT is working on it.
          setChatError(null);
          setTranscribing(true);
        } else {
          setChatError("녹음된 음성이 없어요. 마이크를 누르고 문장을 말해보세요.");
        }
      } catch (err) {
        setMic(false);
        console.error("mic:", err);
        setChatError("녹음을 처리하지 못했어요. 다시 시도하거나 직접 입력해주세요.");
      }
    } else {
      if (voiceDraft) discardVoiceDraft(); // starting over discards the unsent draft
      try {
        await rec.start();
        setChatError(null);
        setMic(true);
      } catch (err) {
        console.error("mic:", err);
        setChatError("마이크를 사용할 수 없어요. 브라우저 권한을 확인하거나 직접 입력해주세요.");
      }
    }
  }, [voiceDraft, transcribing, discardVoiceDraft]);

  // All user turns with feedback worth reviewing, in transcript order — feeds
  // FeedbackSummary. Covers both live turns (correction populated via the WS
  // "correction" event) and hydrated history (populated in enterChat), since
  // both write into the same `turns` map.
  const feedbackTurns = useMemo(
    () =>
      msgs
        .filter((m) => m.role === "user" && !!turns[m.turn]?.correction)
        .map((m) => ({ turn: m.turn, text: m.text, correction: turns[m.turn].correction as Correction }))
        .filter((t) => correctionHasIssues(t.correction)),
    [msgs, turns],
  );

  const playMessage = useCallback(
    (turn: number, role: "user" | "assistant") => {
      if (!activeSessionId) return;
      playAudio(messageAudioURL(activeSessionId, turn, role), playbackRate);
    },
    [activeSessionId, playAudio, playbackRate],
  );

  const submitText = useCallback(() => {
    const t = text.trim();
    if (!t) return;
    clientRef.current?.sendText(t, voiceDraft ? "voice" : undefined);
    setChatError(null);
    setAwaitingReply(true);
    setText("");
    if (activeSessionId) clearDraft(activeSessionId);
    setVoiceDraft(false);
    voiceDraftAutoTextRef.current = null;
    if (quickModeRef.current) setQuickSent(true);
  }, [text, voiceDraft, activeSessionId]);

  const onComposerSubmit = useCallback(
    (e: React.FormEvent) => {
      e.preventDefault();
      submitText();
    },
    [submitText],
  );

  // Enter sends the message; Shift+Enter inserts a newline, matching the
  // usual chat-app convention now that this is a multiline textarea.
  const onComposerKeyDown = useCallback(
    (e: React.KeyboardEvent<HTMLTextAreaElement>) => {
      // Enter confirms an in-progress IME composition on Korean/Japanese
      // keyboards; treating that same keydown as "send" loses the final
      // syllable and submits before the learner intended.
      if (e.nativeEvent.isComposing || e.keyCode === 229) return;
      if (e.key === "Enter" && !e.shiftKey) {
        e.preventDefault();
        submitText();
      }
    },
    [submitText],
  );

  const handleComposerTextChange = useCallback(
    (value: string) => {
      setText(value);
      if (activeSessionId) saveDraft(activeSessionId, value);
      if (voiceDraft && value === "") discardVoiceDraft();
    },
    [activeSessionId, voiceDraft, discardVoiceDraft],
  );

  const textareaRef = useRef<HTMLTextAreaElement>(null);

  // Grow the textarea to fit its content (up to the CSS max-height, which
  // takes over with internal scrolling) so long messages stay fully visible
  // while composing instead of scrolling inside a fixed-height box.
  useEffect(() => {
    const el = textareaRef.current;
    if (!el) return;
    el.style.height = "auto";
    el.style.height = `${el.scrollHeight}px`;
  }, [text]);

  // Relative navigation (not "/recordings"): resolves against the current
  // page URL, so this still works under a ROOT_PATH prefix like "/pr/14"
  // (see lib/route.ts).
  const goTo = useCallback((path: string) => () => window.location.assign(path), []);

  // Click-outside / Escape closes the menu, same as any dropdown.
  const closeMenu = useCallback(() => setMenuOpen(false), []);
  useDismiss(menuOpen, menuRef, closeMenu);

  // Click-outside / Escape closes the grammar-feedback popover, same as
  // the menu.
  const closePanel = useCallback(() => setOpenGrammarIndex(null), []);
  useDismiss(openGrammarIndex !== null, studyRef, closePanel);

  const goToPath = useCallback(
    (e: React.FormEvent) => {
      e.preventDefault();
      const path = prPath(prInput);
      if (path === null) {
        setPrError(true);
        return;
      }
      window.location.assign(path);
    },
    [prInput],
  );

  const handlePrInputChange = useCallback((v: string) => {
    setPrInput(v);
    setPrError(false);
  }, []);

  // Shared by both TopBar calls below (list view and chat view) — only
  // `brand`/`actions`/`chat` differ between them.
  const topBarProps = {
    menuOpen,
    onToggleMenu: () => setMenuOpen((o) => !o),
    menuRef,
    email,
    theme,
    onThemeChange: selectTheme,
    prInput,
    onPrInputChange: handlePrInputChange,
    prError,
    onGoToPath: goToPath,
    onGoToRecordings: goTo("recordings"),
    onGoToInstant: goTo("instant"),
    onGoToWords: goTo("words"),
    onGoToMatch: goTo("match"),
    onGoToArticle: goTo("article"),
    onGoToWriting: goTo("writing"),
    wordDueCount,
    styleInput,
    onStyleInputChange: handleStyleInputChange,
    styleSaving,
    styleSaved,
    styleLoadError,
    styleSaveError,
    onSubmitStyle: submitStyle,
    learnerProfile,
  };

  if (view === "list") {
    return (
      <div className="app">
        <TopBar
          brand={
            <>
              <span className="brand-mark" aria-hidden="true">B</span>
              <h1>Buddy</h1>
            </>
          }
          {...topBarProps}
        />

        <SessionList
          sessions={sessions}
          loading={sessionsLoading}
          loadError={sessionsLoadError}
          actionError={listActionError}
          openingId={openingSessionId}
          deletingId={deletingSessionId}
          onOpen={(id, quick) => void enterChat(id, quick ? { quick: true } : undefined)}
          onDelete={(id) => void handleDeleteSession(id)}
          onRetry={() => void refreshSessions()}
          onDismissError={() => setListActionError(null)}
        />
      </div>
    );
  }

  return (
    <div className="app">
      <TopBar
        brand={
          <>
            <button className="ghost icon-btn" onClick={backToList} aria-label="목록으로" title="목록으로">
              ←
            </button>
            <span
              className={`dot ${ended ? "ended" : status}`}
              role="status"
              aria-label={ended ? "종료된 대화 — 읽기 전용" : CONNECTION_LABELS[status]}
              title={ended ? "종료된 대화 — 읽기 전용" : CONNECTION_LABELS[status]}
            />
            <h1>Buddy</h1>
          </>
        }
        actions={
          <>
            <CompactionInfo sessionId={activeSessionId} />
            <FeedbackSummary turns={feedbackTurns} />
            <EndConversationControl
              sessionId={activeSessionId}
              ended={ended}
              studySummary={endedSummary}
              studySummaryStatus={endedSummaryStatus}
              quiz={endedQuiz}
              quizStatus={endedQuizStatus}
              quizCompleted={endedQuizCompleted}
              onEnd={endConversation}
              onRestudy={restudyConversation}
              onQuizCompleted={handleQuizCompleted}
              onQuizReset={resetConversationQuiz}
            />
          </>
        }
        {...topBarProps}
        chat={{
          autoReadAloud,
          onToggleAutoReadAloud: () => setAutoReadAloud((v) => !v),
          playbackRate,
          onSetPlaybackRate: setPlaybackRate,
        }}
      />

      <audio
        ref={audioRef}
        style={{ display: "none" }}
        onError={() => {
          console.error("tts: playback failed");
          setChatError("음성을 재생하지 못했어요. 잠시 후 다시 시도해주세요.");
        }}
      />

      <ConversationTranscript
        ended={ended}
        status={status}
        loadingMore={loadingMoreHistory}
        quickMode={quickMode}
        messages={msgs}
        turns={turns}
        openGrammarIndex={openGrammarIndex}
        onToggleGrammar={setOpenGrammarIndex}
        grammarPanelRef={studyRef}
        onPlay={playMessage}
        awaitingReply={awaitingReply}
        scrollRef={convoRef}
        latestMessageRef={latestMessageAnchorRef}
        onScroll={handleConvoScroll}
      />

      {showScrollToLatest && !chatError && (
        <button
          type="button"
          className="scroll-to-latest"
          onClick={scrollToLatest}
          aria-label="최신 메시지로 이동"
        >
          ↓ 최신 메시지
        </button>
      )}

      {chatError && (
        <div className="chat-error" role="alert">
          <span>{chatError}</span>
          <button
            type="button"
            className="ghost icon-btn"
            onClick={() => setChatError(null)}
            aria-label="오류 메시지 닫기"
            title="닫기"
          >
            ✕
          </button>
        </div>
      )}

      <MessageComposer
        ended={ended}
        quickMode={quickMode}
        quickSent={quickSent}
        mic={mic}
        transcribing={transcribing}
        text={text}
        voiceDraft={voiceDraft}
        textareaRef={textareaRef}
        onToggleMic={() => void toggleMic()}
        onSubmit={onComposerSubmit}
        onKeyDown={onComposerKeyDown}
        onTextChange={handleComposerTextChange}
        onDiscardVoiceDraft={discardVoiceDraft}
      />
    </div>
  );
}
