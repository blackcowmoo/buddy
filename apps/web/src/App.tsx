import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { BuddyClient, type Status } from "./lib/ws";
import type { Correction, InputSource, ServerEvent } from "./lib/protocol";
import { PCMRecorder } from "./audio/recorder";
import { KokoroSpeaker } from "./tts/kokoro";
import { prPath } from "./lib/rootPath";
import { fetchMe } from "./lib/me";
import { deleteSession, fetchSessionDetail, fetchSessions, type SessionSummary } from "./lib/sessions";
import { applyTheme, getStoredTheme, setStoredTheme, type Theme } from "./lib/theme";
import {
  MAX_EXTRA_RATES,
  NATIVE_RATE,
  isValidExtraRate,
  loadExtraRates,
  saveExtraRates,
} from "./lib/ttsSettings";

interface Msg {
  turn: number;
  role: "user" | "assistant";
  text: string;
  refined?: boolean;
  // How the learner produced this turn — absent for assistant turns.
  source?: InputSource;
}

type TtsState = "idle" | "loading" | "ready" | "error";
type View = "list" | "chat";

// Closes an open dropdown/popover on an outside click or Escape, same
// behavior any of them need — the panel's ref and its own close callback are
// the only per-instance bits.
function useDismiss(open: boolean, ref: React.RefObject<HTMLElement | null>, onClose: () => void) {
  useEffect(() => {
    if (!open) return;
    const onPointerDown = (e: PointerEvent) => {
      if (ref.current && !ref.current.contains(e.target as Node)) onClose();
    };
    const onKeyDown = (e: KeyboardEvent) => {
      if (e.key === "Escape") onClose();
    };
    document.addEventListener("pointerdown", onPointerDown);
    document.addEventListener("keydown", onKeyDown);
    return () => {
      document.removeEventListener("pointerdown", onPointerDown);
      document.removeEventListener("keydown", onKeyDown);
    };
  }, [open, ref, onClose]);
}

export function App() {
  // The home screen always lands on the room list, never a silently
  // reconnected conversation — a WS connection only opens once the learner
  // picks a room or starts a new one (see enterChat).
  const [view, setView] = useState<View>("list");
  const [sessions, setSessions] = useState<SessionSummary[]>([]);
  const [status, setStatus] = useState<Status>("connecting");
  const [msgs, setMsgs] = useState<Msg[]>([]);
  const [corrections, setCorrections] = useState<Record<number, Correction>>({});
  // Turns whose grammar check is still running, tracked only for turns sent
  // during THIS live connection — never set for hydrated history, so an old
  // turn with no saved correction shows as "no data" rather than spinning
  // forever (see enterChat/backToList resets below).
  const [pendingCorrections, setPendingCorrections] = useState<Record<number, boolean>>({});
  // Keyed separately (not one map keyed by turn) because a user turn and its
  // paired assistant reply share the same turn number.
  const [userTranslations, setUserTranslations] = useState<Record<number, string>>({});
  const [assistantTranslations, setAssistantTranslations] = useState<Record<number, string>>({});
  // Turns whose native-language translation hasn't arrived yet — set the
  // moment a translation is expected (final_transcript for the user's own
  // turn, assistant_done for the reply) and, for a hydrated history turn
  // whose translation is still missing, on load too (see enterChat/
  // pollMissingTranslations) so leaving and reopening a room doesn't just
  // silently drop the spinner. Rendering always prefers actual translation
  // text over this flag (see the message list below), so there's no need to
  // explicitly clear an entry once its translation lands.
  const [pendingUserTranslations, setPendingUserTranslations] = useState<Record<number, boolean>>(
    {},
  );
  const [pendingAssistantTranslations, setPendingAssistantTranslations] = useState<
    Record<number, boolean>
  >({});
  // True from the moment a reply is expected (a message was just sent, or a
  // brand-new room was just opened and the server is about to volunteer its
  // opening line) until the first token of that reply arrives — drives the
  // typing indicator in the empty gap before assistant_delta/assistant_done.
  const [awaitingReply, setAwaitingReply] = useState(false);

  // Clears per-turn UI state (corrections, translations) — shared by
  // enterChat (about to load a room's own state, or none for a fresh one)
  // and backToList (leaving the room entirely).
  const resetTurnState = useCallback(() => {
    setCorrections({});
    setPendingCorrections({});
    setUserTranslations({});
    setAssistantTranslations({});
    setPendingUserTranslations({});
    setPendingAssistantTranslations({});
  }, []);

  const [mic, setMic] = useState(false);
  const [text, setText] = useState("");
  const [tts, setTts] = useState<TtsState>("idle");
  const [ttsProgress, setTtsProgress] = useState(0);
  const [menuOpen, setMenuOpen] = useState(false);
  // Which per-row popover (rate study panel or grammar feedback) is open, or
  // null — only one open at a time across the whole row.
  const [openPanel, setOpenPanel] = useState<{ index: number; kind: "rate" | "grammar" } | null>(
    null,
  );
  const [prInput, setPrInput] = useState("");
  const [prError, setPrError] = useState(false);
  const [email, setEmail] = useState<string | null>(null);
  const [theme, setTheme] = useState<Theme>(() => getStoredTheme());
  const [extraRates, setExtraRates] = useState<number[]>(() => loadExtraRates());
  const [newRateInput, setNewRateInput] = useState("");

  const clientRef = useRef<BuddyClient | null>(null);
  const recorderRef = useRef<PCMRecorder | null>(null);
  const speakerRef = useRef<KokoroSpeaker | null>(null);
  const menuRef = useRef<HTMLDivElement>(null);
  const studyRef = useRef<HTMLDivElement>(null);
  // Identifies the most recent backfill-translation poll (see
  // pollMissingTranslations) so a slow fetch that resolves after the learner
  // already left the room, or opened a different one, doesn't apply its
  // (now stale) result to the wrong room's state.
  const pollTokenRef = useRef<object | null>(null);

  const onEvent = useCallback((e: ServerEvent) => {
    switch (e.type) {
      case "ready":
        break;
      case "final_transcript":
        setMsgs((m) => [
          ...m,
          { turn: e.turn, role: "user", text: e.text ?? "", source: e.source },
        ]);
        setPendingCorrections((p) => ({ ...p, [e.turn]: true }));
        setPendingUserTranslations((p) => ({ ...p, [e.turn]: true }));
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
        setPendingAssistantTranslations((p) => ({ ...p, [e.turn]: true }));
        if (e.text && speakerRef.current?.loaded) void speakerRef.current.speak(e.text);
        break;
      case "correction":
        if (e.correction)
          setCorrections((c) => ({ ...c, [e.turn]: e.correction as Correction }));
        setPendingCorrections((p) => {
          if (!(e.turn in p)) return p;
          const next = { ...p };
          delete next[e.turn];
          return next;
        });
        // correct() always emits this once its analyze() pass finishes, even
        // when the sentence needed no teaching — same definitive "done"
        // signal pendingCorrections clears on above, reused here since a
        // clean sentence can still come back with no translation attached.
        setPendingUserTranslations((p) => {
          if (!(e.turn in p)) return p;
          const next = { ...p };
          delete next[e.turn];
          return next;
        });
        break;
      case "user_translation":
        setUserTranslations((t) => ({ ...t, [e.turn]: e.text ?? "" }));
        break;
      case "assistant_translation":
        setAssistantTranslations((t) => ({ ...t, [e.turn]: e.text ?? "" }));
        setPendingAssistantTranslations((p) => {
          if (!(e.turn in p)) return p;
          const next = { ...p };
          delete next[e.turn];
          return next;
        });
        break;
      case "error":
        setAwaitingReply(false);
        console.error("server error:", e.text);
        break;
    }
  }, []);

  useEffect(() => {
    if (clientRef.current) return; // guard StrictMode double-invoke
    // Created eagerly so mic/voice setup and the WS client are ready the
    // moment a room is opened, but connect() is NOT called here — the home
    // screen shows the room list first, not a silently reconnected chat.
    clientRef.current = new BuddyClient(onEvent, setStatus);
    recorderRef.current = new PCMRecorder();
    speakerRef.current = new KokoroSpeaker();
    return () => {
      clientRef.current?.close();
      clientRef.current = null;
    };
  }, [onEvent]);

  useEffect(() => {
    // The CookieIdentifier's id is a random per-browser token, not a real
    // identity, so only oidc mode's id (the verified email claim) is worth
    // showing in the menu.
    fetchMe().then((identity) => {
      setEmail(identity?.identityMode === "oidc" ? identity.id : null);
    });
  }, []);

  const refreshSessions = useCallback(() => {
    fetchSessions().then(setSessions);
  }, []);

  useEffect(() => {
    refreshSessions();
  }, [refreshSessions]);

  // Deletes a chat room from the list without opening it. The server also
  // cascades to any recordings archived under that room (see
  // httpserver.sessionDeleteHandler), so this is the one action that clears
  // both the transcript and its audio.
  const handleDeleteSession = useCallback(async (id: string) => {
    if (!window.confirm("이 대화를 삭제할까요? 저장된 녹음도 함께 삭제됩니다.")) return;
    if (await deleteSession(id)) {
      setSessions((list) => list.filter((s) => s.id !== id));
    }
  }, []);

  // Polls a room's transcript for translations the server is still
  // backfilling in the background (see internal/backfill) — opening a room
  // only fetches its transcript once, and backfill has no push channel to
  // tell an already-open client "it's ready now", so without this a turn
  // still missing its translation would just show a spinner that never
  // resolves until the learner leaves and reopens the room. Stops once
  // nothing is missing anymore or maxAttempts is reached; a fresh call to
  // enterChat/backToList invalidates `token` so a slow, late-arriving
  // response never overwrites a different room's state.
  const pollMissingTranslations = useCallback((sessionId: string, token: object) => {
    const maxAttempts = 20;
    const intervalMs = 4000;
    let attempt = 0;
    const tick = async () => {
      if (pollTokenRef.current !== token) return; // left this room, or opened another
      attempt++;
      const detail = await fetchSessionDetail(sessionId);
      if (pollTokenRef.current !== token || !detail) return;
      let stillMissing = false;
      const ut: Record<number, string> = {};
      const at: Record<number, string> = {};
      for (const t of detail.turns) {
        if (t.translation) {
          if (t.role === "user") ut[t.turn] = t.translation;
          else at[t.turn] = t.translation;
        } else if (t.text) {
          stillMissing = true;
        }
      }
      setUserTranslations((prev) => ({ ...prev, ...ut }));
      setAssistantTranslations((prev) => ({ ...prev, ...at }));
      if (stillMissing && attempt < maxAttempts) setTimeout(tick, intervalMs);
    };
    setTimeout(tick, intervalMs);
  }, []);

  // Opens a room and enters chat view. sessionId omitted starts a brand-new
  // room (server mints the ID, delivered on the "ready" event); given an
  // existing ID, this hydrates the visible transcript from its persisted
  // history first, since reconnecting the WS alone only seeds LLM context,
  // it doesn't replay old chat bubbles.
  const enterChat = useCallback(
    async (sessionId?: string) => {
      resetTurnState();
      setAwaitingReply(false);
      const token = {};
      pollTokenRef.current = token;
      if (sessionId) {
        // Fire the WS handshake alongside the transcript fetch — they're
        // independent round trips — instead of waiting for the fetch first.
        clientRef.current?.connect(sessionId);
        const detail = await fetchSessionDetail(sessionId);
        if (!detail) {
          clientRef.current?.close(); // fetch failed (e.g. deleted elsewhere) — stay on the list
          return;
        }
        setMsgs(
          detail.turns.map((t) => ({
            turn: t.turn,
            role: t.role,
            text: t.text,
            refined: t.refined,
            source: t.source,
          })),
        );
        const corr: Record<number, Correction> = {};
        const ut: Record<number, string> = {};
        const at: Record<number, string> = {};
        const pendingUt: Record<number, boolean> = {};
        const pendingAt: Record<number, boolean> = {};
        for (const t of detail.turns) {
          if (t.correction) corr[t.turn] = t.correction;
          if (t.translation) {
            if (t.role === "user") ut[t.turn] = t.translation;
            else at[t.turn] = t.translation;
          } else if (t.text) {
            // Missing translation on a hydrated turn: the server queues
            // backfill for it the moment this fetch lands (see
            // httpserver.sessionDetailHandler), so show it as in-progress
            // rather than silently absent, and poll until it lands.
            if (t.role === "user") pendingUt[t.turn] = true;
            else pendingAt[t.turn] = true;
          }
        }
        setCorrections(corr);
        setUserTranslations(ut);
        setAssistantTranslations(at);
        setPendingUserTranslations(pendingUt);
        setPendingAssistantTranslations(pendingAt);
        if (Object.keys(pendingUt).length > 0 || Object.keys(pendingAt).length > 0) {
          pollMissingTranslations(sessionId, token);
        }
      } else {
        setMsgs([]);
        clientRef.current?.connect(undefined);
        // A brand-new room gets an opening line from the server before the
        // learner says anything (see pipeline.StartConversation) — show the
        // typing indicator right away instead of a bare empty screen.
        setAwaitingReply(true);
      }
      setMenuOpen(false);
      setView("chat");
    },
    [resetTurnState, pollMissingTranslations],
  );

  const backToList = useCallback(() => {
    pollTokenRef.current = null;
    clientRef.current?.close();
    setMsgs([]);
    resetTurnState();
    setAwaitingReply(false);
    setMenuOpen(false);
    setView("list");
    refreshSessions();
  }, [refreshSessions, resetTurnState]);

  useEffect(() => {
    applyTheme(theme);
    if (theme !== "system") return;
    // Live-follow OS/browser theme changes while "system" is selected,
    // instead of only resolving once at mount.
    const mql = window.matchMedia("(prefers-color-scheme: light)");
    const onChange = () => applyTheme("system");
    mql.addEventListener("change", onChange);
    return () => mql.removeEventListener("change", onChange);
  }, [theme]);

  const selectTheme = useCallback((t: Theme) => {
    setStoredTheme(t);
    setTheme(t);
  }, []);

  // A dropped connection can never deliver the reply the learner is waiting
  // on, so don't leave the typing indicator spinning forever.
  useEffect(() => {
    if (status === "closed" || status === "error") setAwaitingReply(false);
  }, [status]);

  const toggleMic = useCallback(async () => {
    const rec = recorderRef.current;
    if (!rec) return;
    if (rec.isRecording) {
      const pcm = await rec.stop();
      setMic(false);
      if (pcm.length > 0) {
        clientRef.current?.sendAudio(pcm);
        setAwaitingReply(true);
      }
    } else {
      try {
        await rec.start();
        setMic(true);
      } catch (err) {
        console.error("mic:", err);
      }
    }
  }, []);

  const loadVoice = useCallback(async () => {
    const sp = speakerRef.current;
    if (!sp) return;
    setTts("loading");
    try {
      await sp.load((p) => setTtsProgress(p));
      setTts("ready");
    } catch (err) {
      console.error("kokoro:", err);
      setTts("error");
    }
  }, []);

  useEffect(() => {
    saveExtraRates(extraRates);
  }, [extraRates]);

  // Ascending so the fastest speed is always last; native (1.0) sorts
  // wherever it falls relative to whatever custom speeds are configured.
  const playRates = useMemo(
    () => [...extraRates, NATIVE_RATE].sort((a, b) => a - b),
    [extraRates],
  );

  // All user turns with feedback worth reviewing, in transcript order — feeds
  // FeedbackSummary. Covers both live turns (corrections populated via the WS
  // "correction" event) and hydrated history (populated in enterChat), since
  // both write into the same `corrections` map.
  const feedbackTurns = useMemo(
    () =>
      msgs
        .filter((m) => m.role === "user" && !!corrections[m.turn])
        .map((m) => ({ turn: m.turn, text: m.text, correction: corrections[m.turn] }))
        .filter((t) => correctionHasIssues(t.correction)),
    [msgs, corrections],
  );

  const playMessage = useCallback(
    async (text: string, rate: number) => {
      const sp = speakerRef.current;
      if (!sp) return;
      if (!sp.loaded) await loadVoice();
      void sp.speak(text, rate);
    },
    [loadVoice],
  );

  const addRate = useCallback(
    (e: React.FormEvent) => {
      e.preventDefault();
      const v = Number(newRateInput);
      if (!isValidExtraRate(v)) return;
      setExtraRates((rates) => {
        if (rates.length >= MAX_EXTRA_RATES || rates.includes(v)) return rates;
        return [...rates, v].sort((a, b) => a - b);
      });
      setNewRateInput("");
    },
    [newRateInput],
  );

  const removeRate = useCallback((rate: number) => {
    setExtraRates((rates) => rates.filter((r) => r !== rate));
  }, []);

  const submitText = useCallback(
    (e: React.FormEvent) => {
      e.preventDefault();
      const t = text.trim();
      if (!t) return;
      clientRef.current?.sendText(t);
      setAwaitingReply(true);
      setText("");
    },
    [text],
  );

  const goToRecordings = useCallback(() => {
    // Relative navigation (not "/recordings"): resolves against the current
    // page URL, so this still works under a ROOT_PATH prefix like "/pr/14"
    // (see lib/route.ts).
    window.location.assign("recordings");
  }, []);

  // Click-outside / Escape closes the menu, same as any dropdown.
  const closeMenu = useCallback(() => setMenuOpen(false), []);
  useDismiss(menuOpen, menuRef, closeMenu);

  // Click-outside / Escape closes whichever per-row popover is open, same as
  // the menu.
  const closePanel = useCallback(() => setOpenPanel(null), []);
  useDismiss(openPanel !== null, studyRef, closePanel);

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

  if (view === "list") {
    return (
      <div className="app">
        <TopBar
          brand={<h1>Buddy</h1>}
          menuOpen={menuOpen}
          onToggleMenu={() => setMenuOpen((o) => !o)}
          menuRef={menuRef}
          email={email}
          theme={theme}
          onThemeChange={selectTheme}
          prInput={prInput}
          onPrInputChange={handlePrInputChange}
          prError={prError}
          onGoToPath={goToPath}
          onGoToRecordings={goToRecordings}
        />

        <main className="session-list">
          <button className="new-chat" onClick={() => void enterChat()}>
            + 새 대화
          </button>
          {sessions.length === 0 ? (
            <p className="hint">아직 대화 기록이 없어요. 새 대화를 시작해보세요.</p>
          ) : (
            <ul>
              {sessions.map((s) => (
                <li key={s.id} className="session-row">
                  <button className="session-item" onClick={() => void enterChat(s.id)}>
                    <span className="title">{s.title}</span>
                    <span className="time">{formatRelativeTime(s.updatedAt)}</span>
                  </button>
                  <button
                    type="button"
                    className="ghost icon-btn session-delete"
                    onClick={() => void handleDeleteSession(s.id)}
                    aria-label="대화 삭제"
                    title="대화 삭제"
                  >
                    🗑
                  </button>
                </li>
              ))}
            </ul>
          )}
        </main>
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
            <span className={`dot ${status}`} title={status} />
            <h1>Buddy</h1>
          </>
        }
        actions={<FeedbackSummary turns={feedbackTurns} />}
        menuOpen={menuOpen}
        onToggleMenu={() => setMenuOpen((o) => !o)}
        menuRef={menuRef}
        email={email}
        theme={theme}
        onThemeChange={selectTheme}
        prInput={prInput}
        onPrInputChange={handlePrInputChange}
        prError={prError}
        onGoToPath={goToPath}
        onGoToRecordings={goToRecordings}
        chat={{
          tts,
          ttsProgress,
          onLoadVoice: loadVoice,
          extraRates,
          newRateInput,
          onNewRateInputChange: setNewRateInput,
          onAddRate: addRate,
          onRemoveRate: removeRate,
        }}
      />

      <main className="convo">
        {msgs.length === 0 && (
          <p className="hint">
            Tap <strong>Enable voice</strong> to load kokoro, tap the{" "}
            <strong>🎙</strong> button, speak a sentence, then tap it again to
            send. Or just type below.
          </p>
        )}
        {msgs.map((m, i) => {
          const translation =
            m.role === "user" ? userTranslations[m.turn] : assistantTranslations[m.turn];
          const translationPending =
            m.role === "user" ? pendingUserTranslations[m.turn] : pendingAssistantTranslations[m.turn];
          return (
            <div key={i} className={`row ${m.role}`}>
              <div className="bubble">
                {m.text || <span className="cursor">▋</span>}
                {m.role === "user" && m.source && (
                  <span
                    className="source-icon"
                    title={m.source === "voice" ? "음성으로 입력함" : "채팅으로 입력함"}
                  >
                    {m.source === "voice" ? "🎙" : "⌨️"}
                  </span>
                )}
                {m.role === "user" && m.refined && <span className="tag">refined</span>}
              </div>
              {translation ? (
                <div className="translation">{translation}</div>
              ) : (
                translationPending && (
                  <div className="translation translation-pending" role="status" aria-label="번역 중">
                    <span className="spinning">⏳</span>
                  </div>
                )
              )}
              {m.text && (
                <div className="msg-tools">
                  {m.role === "user" && (
                    <GrammarControl
                      index={i}
                      pending={!!pendingCorrections[m.turn]}
                      correction={corrections[m.turn]}
                      open={openPanel?.index === i && openPanel.kind === "grammar"}
                      onToggle={(idx) =>
                        setOpenPanel(idx === null ? null : { index: idx, kind: "grammar" })
                      }
                      panelRef={
                        openPanel?.index === i && openPanel.kind === "grammar" ? studyRef : undefined
                      }
                    />
                  )}
                  <StudyControl
                    index={i}
                    text={m.text}
                    rates={playRates}
                    open={openPanel?.index === i && openPanel.kind === "rate"}
                    onToggle={(idx) =>
                      setOpenPanel(idx === null ? null : { index: idx, kind: "rate" })
                    }
                    onPlay={playMessage}
                    panelRef={
                      openPanel?.index === i && openPanel.kind === "rate" ? studyRef : undefined
                    }
                  />
                </div>
              )}
            </div>
          );
        })}
        {awaitingReply && (
          <div className="row assistant">
            <div className="bubble typing" role="status" aria-label="답변 생성 중">
              <span className="dot" />
              <span className="dot" />
              <span className="dot" />
            </div>
          </div>
        )}
      </main>

      <footer className="composer">
        <button
          className={`mic ${mic ? "on" : ""}`}
          onClick={toggleMic}
          aria-label={mic ? "Stop recording" : "Push to talk"}
          aria-pressed={mic}
          title="Push to talk"
        >
          {mic ? "◼" : "🎙"}
        </button>
        <form onSubmit={submitText}>
          <input
            value={text}
            onChange={(e) => setText(e.target.value)}
            placeholder="…or type in English"
            enterKeyHint="send"
            autoComplete="off"
            autoCorrect="on"
          />
          <button type="submit">Send</button>
        </form>
      </footer>
    </div>
  );
}

function formatRelativeTime(unixSeconds: number): string {
  const mins = Math.floor((Date.now() - unixSeconds * 1000) / 60000);
  if (mins < 1) return "방금 전";
  if (mins < 60) return `${mins}분 전`;
  const hours = Math.floor(mins / 60);
  if (hours < 24) return `${hours}시간 전`;
  const days = Math.floor(hours / 24);
  if (days < 30) return `${days}일 전`;
  return new Date(unixSeconds * 1000).toLocaleDateString();
}

function upsertAssistant(m: Msg[], turn: number, patch: (prev: string) => string): Msg[] {
  const i = m.findIndex((x) => x.turn === turn && x.role === "assistant");
  if (i >= 0) {
    const copy = [...m];
    copy[i] = { ...copy[i], text: patch(copy[i].text) };
    return copy;
  }
  return [...m, { turn, role: "assistant", text: patch("") }];
}

// Whether a Correction actually flags something worth showing, vs. the
// sentence coming back unchanged with no issues (the "clean" case).
function correctionHasIssues(c: Correction): boolean {
  return (
    c.corrected.trim().toLowerCase() !== c.original.trim().toLowerCase() ||
    (c.issues?.length ?? 0) > 0
  );
}

// GrammarControl collapses the background grammar-check result into one
// small button, next to StudyControl's 🔊, instead of an always-visible card:
// it spins while correct() is still running for this turn, then opens a
// popover with the CorrectionCard (or a "no issues" message) on click.
function GrammarControl({
  index,
  pending,
  correction,
  open,
  onToggle,
  panelRef,
}: {
  index: number;
  pending: boolean;
  correction?: Correction;
  open: boolean;
  onToggle: (index: number | null) => void;
  panelRef?: React.RefObject<HTMLDivElement | null>;
}) {
  if (!pending && !correction) return null; // no data (e.g. old session predating this feature)

  const hasIssues = !!correction && correctionHasIssues(correction);

  const glyph = pending ? "⏳" : hasIssues ? "✎" : "✓";
  const label = pending
    ? "문법 확인 중"
    : hasIssues
      ? "문법 피드백 열기"
      : "문법 피드백 열기 (문제 없음)";

  return (
    <div className="grammar-control" ref={panelRef}>
      <button
        type="button"
        className="ghost icon-btn grammar-btn"
        aria-haspopup="true"
        aria-expanded={open}
        aria-busy={pending}
        aria-label={label}
        disabled={pending}
        onClick={() => onToggle(open ? null : index)}
      >
        <span className={pending ? "spinning" : undefined}>{glyph}</span>
      </button>
      {open && correction && (
        <div className="study-panel grammar-panel" role="menu">
          {hasIssues ? (
            <CorrectionCard c={correction} />
          ) : (
            <div className="grammar-clean">문법 문제가 없어요 👍</div>
          )}
        </div>
      )}
    </div>
  );
}

// StudyControl collapses per-rate playback into one small button that opens
// a popover, kept separate from the bubble/correction card so it doesn't
// compete with them for visual weight.
function StudyControl({
  index,
  text,
  rates,
  open,
  onToggle,
  onPlay,
  panelRef,
}: {
  index: number;
  text: string;
  rates: number[];
  open: boolean;
  onToggle: (index: number | null) => void;
  onPlay: (text: string, rate: number) => void;
  panelRef?: React.RefObject<HTMLDivElement | null>;
}) {
  return (
    <div className="study-control" ref={panelRef}>
      <button
        type="button"
        className="ghost icon-btn study-btn"
        aria-haspopup="true"
        aria-expanded={open}
        aria-label="발음 연습 열기"
        onClick={() => onToggle(open ? null : index)}
      >
        🔊
      </button>
      {open && (
        <div className="study-panel" role="menu">
          <div className="tts-controls">
            {rates.map((rate) => (
              <button
                key={rate}
                type="button"
                className="ghost tts-btn"
                onClick={() => onPlay(text, rate)}
                aria-label={
                  rate === NATIVE_RATE ? `${rate}배속(원어민 속도)으로 재생` : `${rate}배속으로 재생`
                }
              >
                {rate === NATIVE_RATE ? `🔊 ${rate}x` : `${rate}x`}
              </button>
            ))}
          </div>
        </div>
      )}
    </div>
  );
}

function VoiceButton({
  state,
  progress,
  onLoad,
}: {
  state: TtsState;
  progress: number;
  onLoad: () => void;
}) {
  if (state === "ready") return <span className="pill ok">🔊 voice ready</span>;
  if (state === "loading")
    return <span className="pill">loading… {Math.round(progress)}%</span>;
  if (state === "error") return <span className="pill err">voice failed</span>;
  return (
    <button className="ghost" onClick={onLoad}>
      Enable voice
    </button>
  );
}

const THEME_LABELS: Record<Theme, string> = {
  white: "화이트",
  dark: "다크",
  system: "시스템 설정",
};

function ThemeSwitch({ theme, onChange }: { theme: Theme; onChange: (t: Theme) => void }) {
  return (
    <div className="theme-switch" role="group" aria-label="테마">
      {(Object.keys(THEME_LABELS) as Theme[]).map((t) => (
        <button
          key={t}
          type="button"
          className={`theme-option ${theme === t ? "active" : ""}`}
          aria-pressed={theme === t}
          onClick={() => onChange(t)}
        >
          {THEME_LABELS[t]}
        </button>
      ))}
    </div>
  );
}

interface ChatMenuProps {
  tts: TtsState;
  ttsProgress: number;
  onLoadVoice: () => void;
  extraRates: number[];
  newRateInput: string;
  onNewRateInputChange: (v: string) => void;
  onAddRate: (e: React.FormEvent) => void;
  onRemoveRate: (rate: number) => void;
}

// Shared by both the room list and the chat header (see App's two return
// branches) — only the brand content (back button + status dot vs. just the
// title) and whether chat-only MenuPanel items are shown differ between them.
function TopBar({
  brand,
  actions,
  menuOpen,
  onToggleMenu,
  menuRef,
  ...menuPanelProps
}: {
  brand: React.ReactNode;
  actions?: React.ReactNode;
  menuOpen: boolean;
  onToggleMenu: () => void;
  menuRef: React.RefObject<HTMLDivElement | null>;
} & React.ComponentProps<typeof MenuPanel>) {
  return (
    <header className="topbar">
      <div className="brand">{brand}</div>
      <div className="menu" ref={menuRef}>
        {actions}
        <button
          className="ghost icon-btn"
          onClick={onToggleMenu}
          aria-label="Menu"
          aria-haspopup="true"
          aria-expanded={menuOpen}
          title="Menu"
        >
          ☰
        </button>
        {menuOpen && <MenuPanel {...menuPanelProps} />}
      </div>
    </header>
  );
}

// Identity, theme, and PR-preview nav — reachable via TopBar before a room is
// ever opened. `chat` carries the items that only make sense once a room is
// open (voice/playback speed) — omitted on the list.
function MenuPanel({
  email,
  theme,
  onThemeChange,
  prInput,
  onPrInputChange,
  prError,
  onGoToPath,
  onGoToRecordings,
  chat,
}: {
  email: string | null;
  theme: Theme;
  onThemeChange: (t: Theme) => void;
  prInput: string;
  onPrInputChange: (v: string) => void;
  prError: boolean;
  onGoToPath: (e: React.FormEvent) => void;
  onGoToRecordings: () => void;
  chat?: ChatMenuProps;
}) {
  return (
    <div className="menu-panel" role="menu">
      <div className="menu-row user-info">
        <span className="user-email">{email ?? "익명 사용자"}</span>
      </div>
      <div className="menu-divider" />
      {chat && (
        <>
          <div className="menu-row">
            <VoiceButton state={chat.tts} progress={chat.ttsProgress} onLoad={chat.onLoadVoice} />
          </div>
          <div className="menu-row tts-settings">
            <div className="tts-settings-label">재생 속도</div>
            <div className="rate-chips">
              <span className="rate-chip locked">🔊 {NATIVE_RATE}x (원어민)</span>
              {chat.extraRates.map((r) => (
                <span key={r} className="rate-chip">
                  {r}x
                  <button
                    type="button"
                    className="chip-remove"
                    onClick={() => chat.onRemoveRate(r)}
                    aria-label={`${r}x 속도 삭제`}
                  >
                    ×
                  </button>
                </span>
              ))}
            </div>
            {chat.extraRates.length < MAX_EXTRA_RATES && (
              <form className="rate-add-form" onSubmit={chat.onAddRate}>
                <input
                  type="number"
                  step="0.05"
                  min="0.5"
                  max="2"
                  value={chat.newRateInput}
                  onChange={(e) => chat.onNewRateInputChange(e.target.value)}
                  placeholder="예: 0.7"
                  aria-label="새 재생 속도"
                />
                <button type="submit">추가</button>
              </form>
            )}
          </div>
          <div className="menu-divider" />
        </>
      )}
      <div className="menu-row">
        <ThemeSwitch theme={theme} onChange={onThemeChange} />
      </div>
      <button className="ghost menu-item" onClick={onGoToRecordings} role="menuitem">
        🎧 녹음 목록
      </button>
      <div className="menu-divider" />
      <form className="path-form" onSubmit={onGoToPath}>
        <label htmlFor="pr-path">PR 미리보기로 이동</label>
        <div className="path-row">
          <input
            id="pr-path"
            value={prInput}
            onChange={(e) => onPrInputChange(e.target.value)}
            placeholder="PR 번호 (예: 14)"
            inputMode="numeric"
            autoComplete="off"
          />
          <button type="submit">이동</button>
        </div>
        {prError && <p className="path-error">숫자만 입력하세요 (비워두면 메인으로 이동)</p>}
      </form>
    </div>
  );
}

// Static UI labels for issue categories, in Korean. No LLM needed — the
// category set is fixed by the correction prompt.
const ISSUE_LABELS: Record<string, string> = {
  grammar: "문법",
  vocabulary: "어휘",
  phrasing: "표현",
  context: "문맥",
};

function issueLabel(type: string): string {
  return ISSUE_LABELS[type] ?? type;
}

function CorrectionCard({ c }: { c: Correction }) {
  const changed = c.corrected.trim() && c.corrected.trim() !== c.original.trim();
  return (
    <div className="correction">
      {changed && (
        <div className="fix">
          <span className="lbl">✎</span> {c.corrected}
        </div>
      )}
      {c.issues?.map((iss, i) => (
        <div key={i} className="issue">
          <span className={`badge ${iss.type}`}>{issueLabel(iss.type)}</span>
          <span className="span">{iss.span}</span> → <b>{iss.suggestion}</b>
          <div className="why">{iss.explanation}</div>
        </div>
      ))}
    </div>
  );
}

interface FeedbackTurn {
  turn: number;
  text: string;
  correction: Correction;
}

// Session-wide counterpart to GrammarControl: instead of one popover per
// message, this is a single button (placed in the chat header) that lists
// every turn's feedback collected so far, so a learner can review recurring
// mistakes mid-conversation instead of only one bubble at a time. Owns its
// own open state (like GrammarControl/StudyControl) since it isn't tied to
// any one message row.
function FeedbackSummary({ turns }: { turns: FeedbackTurn[] }) {
  const [open, setOpen] = useState(false);
  const panelRef = useRef<HTMLDivElement>(null);
  useDismiss(open, panelRef, () => setOpen(false));

  const issueCounts = useMemo(() => {
    const counts: Record<string, number> = {};
    for (const t of turns) {
      for (const iss of t.correction.issues ?? []) {
        counts[iss.type] = (counts[iss.type] ?? 0) + 1;
      }
    }
    return counts;
  }, [turns]);

  return (
    <div className="feedback-summary" ref={panelRef}>
      <button
        type="button"
        className="ghost icon-btn feedback-btn"
        aria-haspopup="true"
        aria-expanded={open}
        aria-label="피드백 모아보기"
        title="피드백 모아보기"
        onClick={() => setOpen((o) => !o)}
      >
        📋
      </button>
      {open && (
        <div className="study-panel feedback-panel" role="menu">
          {turns.length === 0 ? (
            <div className="feedback-empty">아직 피드백이 없어요 👍</div>
          ) : (
            <>
              <div className="feedback-summary-header">
                지금까지 {turns.length}개 메시지에 피드백이 있어요
              </div>
              {Object.keys(issueCounts).length > 0 && (
                <div className="feedback-summary-counts">
                  {Object.entries(issueCounts).map(([type, count]) => (
                    <span key={type} className={`badge ${type}`}>
                      {issueLabel(type)} {count}
                    </span>
                  ))}
                </div>
              )}
              {turns.map((t) => (
                <div key={t.turn} className="feedback-entry">
                  <div className="feedback-original">{t.text}</div>
                  <CorrectionCard c={t.correction} />
                </div>
              ))}
            </>
          )}
        </div>
      )}
    </div>
  );
}
