import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { BuddyClient, type Status } from "./lib/ws";
import type { Correction, ServerEvent } from "./lib/protocol";
import { PCMRecorder } from "./audio/recorder";
import { KokoroSpeaker } from "./tts/kokoro";
import { prPath } from "./lib/rootPath";
import { fetchMe } from "./lib/me";
import { fetchSessionDetail, fetchSessions, type SessionSummary } from "./lib/sessions";
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
}

type TtsState = "idle" | "loading" | "ready" | "error";
type View = "list" | "chat";

export function App() {
  // The home screen always lands on the room list, never a silently
  // reconnected conversation — a WS connection only opens once the learner
  // picks a room or starts a new one (see enterChat).
  const [view, setView] = useState<View>("list");
  const [sessions, setSessions] = useState<SessionSummary[]>([]);
  const [status, setStatus] = useState<Status>("connecting");
  const [msgs, setMsgs] = useState<Msg[]>([]);
  const [corrections, setCorrections] = useState<Record<number, Correction>>({});
  const [mic, setMic] = useState(false);
  const [text, setText] = useState("");
  const [tts, setTts] = useState<TtsState>("idle");
  const [ttsProgress, setTtsProgress] = useState(0);
  const [menuOpen, setMenuOpen] = useState(false);
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

  const onEvent = useCallback((e: ServerEvent) => {
    switch (e.type) {
      case "ready":
        break;
      case "final_transcript":
        setMsgs((m) => [...m, { turn: e.turn, role: "user", text: e.text ?? "" }]);
        break;
      case "refined_transcript":
        setMsgs((m) =>
          m.map((x) =>
            x.turn === e.turn && x.role === "user"
              ? { ...x, text: e.text ?? x.text, refined: true }
              : x,
          ),
        );
        break;
      case "assistant_delta":
        setMsgs((m) => upsertAssistant(m, e.turn, (prev) => prev + (e.text ?? "")));
        break;
      case "assistant_done":
        setMsgs((m) => upsertAssistant(m, e.turn, () => e.text ?? ""));
        if (e.text && speakerRef.current?.loaded) void speakerRef.current.speak(e.text);
        break;
      case "correction":
        if (e.correction)
          setCorrections((c) => ({ ...c, [e.turn]: e.correction as Correction }));
        break;
      case "error":
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

  // Opens a room and enters chat view. sessionId omitted starts a brand-new
  // room (server mints the ID, delivered on the "ready" event); given an
  // existing ID, this hydrates the visible transcript from its persisted
  // history first, since reconnecting the WS alone only seeds LLM context,
  // it doesn't replay old chat bubbles.
  const enterChat = useCallback(async (sessionId?: string) => {
    setCorrections({});
    if (sessionId) {
      // Fire the WS handshake alongside the transcript fetch — they're
      // independent round trips — instead of waiting for the fetch first.
      clientRef.current?.connect(sessionId);
      const detail = await fetchSessionDetail(sessionId);
      if (!detail) {
        clientRef.current?.close(); // fetch failed (e.g. deleted elsewhere) — stay on the list
        return;
      }
      setMsgs(detail.turns.map((t) => ({ turn: t.turn, role: t.role, text: t.text, refined: t.refined })));
      const corr: Record<number, Correction> = {};
      for (const t of detail.turns) if (t.correction) corr[t.turn] = t.correction;
      setCorrections(corr);
    } else {
      setMsgs([]);
      clientRef.current?.connect(undefined);
    }
    setMenuOpen(false);
    setView("chat");
  }, []);

  const backToList = useCallback(() => {
    clientRef.current?.close();
    setMsgs([]);
    setCorrections({});
    setMenuOpen(false);
    setView("list");
    refreshSessions();
  }, [refreshSessions]);

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

  const toggleMic = useCallback(async () => {
    const rec = recorderRef.current;
    if (!rec) return;
    if (rec.isRecording) {
      const pcm = await rec.stop();
      setMic(false);
      if (pcm.length > 0) clientRef.current?.sendAudio(pcm);
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
      setText("");
    },
    [text],
  );

  // Click-outside / Escape closes the menu, same as any dropdown.
  useEffect(() => {
    if (!menuOpen) return;
    const onPointerDown = (e: PointerEvent) => {
      if (menuRef.current && !menuRef.current.contains(e.target as Node)) setMenuOpen(false);
    };
    const onKeyDown = (e: KeyboardEvent) => {
      if (e.key === "Escape") setMenuOpen(false);
    };
    document.addEventListener("pointerdown", onPointerDown);
    document.addEventListener("keydown", onKeyDown);
    return () => {
      document.removeEventListener("pointerdown", onPointerDown);
      document.removeEventListener("keydown", onKeyDown);
    };
  }, [menuOpen]);

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
        <header className="topbar">
          <div className="brand">
            <h1>Buddy</h1>
          </div>
          <div className="menu" ref={menuRef}>
            <button
              className="ghost icon-btn"
              onClick={() => setMenuOpen((o) => !o)}
              aria-label="Menu"
              aria-haspopup="true"
              aria-expanded={menuOpen}
              title="Menu"
            >
              ☰
            </button>
            {menuOpen && (
              <MenuPanel
                email={email}
                theme={theme}
                onThemeChange={selectTheme}
                prInput={prInput}
                onPrInputChange={handlePrInputChange}
                prError={prError}
                onGoToPath={goToPath}
              />
            )}
          </div>
        </header>

        <main className="session-list">
          <button className="new-chat" onClick={() => void enterChat()}>
            + 새 대화
          </button>
          {sessions.length === 0 ? (
            <p className="hint">아직 대화 기록이 없어요. 새 대화를 시작해보세요.</p>
          ) : (
            <ul>
              {sessions.map((s) => (
                <li key={s.id}>
                  <button className="session-item" onClick={() => void enterChat(s.id)}>
                    <span className="title">{s.title}</span>
                    <span className="time">{formatRelativeTime(s.updatedAt)}</span>
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
      <header className="topbar">
        <div className="brand">
          <button className="ghost icon-btn" onClick={backToList} aria-label="목록으로" title="목록으로">
            ←
          </button>
          <span className={`dot ${status}`} title={status} />
          <h1>Buddy</h1>
        </div>
        <div className="menu" ref={menuRef}>
          <button
            className="ghost icon-btn"
            onClick={() => setMenuOpen((o) => !o)}
            aria-label="Menu"
            aria-haspopup="true"
            aria-expanded={menuOpen}
            title="Menu"
          >
            ☰
          </button>
          {menuOpen && (
            <MenuPanel
              email={email}
              theme={theme}
              onThemeChange={selectTheme}
              prInput={prInput}
              onPrInputChange={handlePrInputChange}
              prError={prError}
              onGoToPath={goToPath}
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
          )}
        </div>
      </header>

      <main className="convo">
        {msgs.length === 0 && (
          <p className="hint">
            Tap <strong>Enable voice</strong> to load kokoro, tap the{" "}
            <strong>🎙</strong> button, speak a sentence, then tap it again to
            send. Or just type below.
          </p>
        )}
        {msgs.map((m, i) => (
          <div key={i} className={`row ${m.role}`}>
            <div className="bubble">
              {m.text || <span className="cursor">▋</span>}
              {m.role === "user" && m.refined && <span className="tag">refined</span>}
            </div>
            {m.role === "user" && corrections[m.turn] && (
              <CorrectionCard c={corrections[m.turn]} />
            )}
            {m.text && <PlayButtons text={m.text} rates={playRates} onPlay={playMessage} />}
          </div>
        ))}
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

function PlayButtons({
  text,
  rates,
  onPlay,
}: {
  text: string;
  rates: number[];
  onPlay: (text: string, rate: number) => void;
}) {
  return (
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

// Shared by both the room list and the chat header, so the menu (identity,
// theme, PR-preview nav) is reachable before a room is ever opened, not just
// from within a conversation. `chat` carries the items that only make sense
// once a room is open (voice/playback speed) — omitted on the list.
function MenuPanel({
  email,
  theme,
  onThemeChange,
  prInput,
  onPrInputChange,
  prError,
  onGoToPath,
  chat,
}: {
  email: string | null;
  theme: Theme;
  onThemeChange: (t: Theme) => void;
  prInput: string;
  onPrInputChange: (v: string) => void;
  prError: boolean;
  onGoToPath: (e: React.FormEvent) => void;
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
