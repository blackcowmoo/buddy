import { useCallback, useEffect, useRef, useState } from "react";
import { BuddyClient, type Status } from "./lib/ws";
import type { Correction, ServerEvent } from "./lib/protocol";
import { PCMRecorder } from "./audio/recorder";
import { KokoroSpeaker } from "./tts/kokoro";

interface Msg {
  turn: number;
  role: "user" | "assistant";
  text: string;
  refined?: boolean;
}

type TtsState = "idle" | "loading" | "ready" | "error";

export function App() {
  const [status, setStatus] = useState<Status>("connecting");
  const [msgs, setMsgs] = useState<Msg[]>([]);
  const [corrections, setCorrections] = useState<Record<number, Correction>>({});
  const [mic, setMic] = useState(false);
  const [text, setText] = useState("");
  const [tts, setTts] = useState<TtsState>("idle");
  const [ttsProgress, setTtsProgress] = useState(0);

  const clientRef = useRef<BuddyClient | null>(null);
  const recorderRef = useRef<PCMRecorder | null>(null);
  const speakerRef = useRef<KokoroSpeaker | null>(null);

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
    const client = new BuddyClient(onEvent, setStatus);
    clientRef.current = client;
    recorderRef.current = new PCMRecorder();
    speakerRef.current = new KokoroSpeaker();
    client.connect();
    return () => {
      client.close();
      clientRef.current = null;
    };
  }, [onEvent]);

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

  const resetChat = useCallback(() => {
    clientRef.current?.reset();
    setMsgs([]);
    setCorrections({});
  }, []);

  return (
    <div className="app">
      <header className="topbar">
        <div className="brand">
          <span className={`dot ${status}`} title={status} />
          <h1>Buddy</h1>
        </div>
        <div className="controls">
          <VoiceButton state={tts} progress={ttsProgress} onLoad={loadVoice} />
          <button
            className="ghost icon-btn"
            onClick={resetChat}
            aria-label="Reset conversation"
            title="Reset"
          >
            ↺
          </button>
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

function upsertAssistant(m: Msg[], turn: number, patch: (prev: string) => string): Msg[] {
  const i = m.findIndex((x) => x.turn === turn && x.role === "assistant");
  if (i >= 0) {
    const copy = [...m];
    copy[i] = { ...copy[i], text: patch(copy[i].text) };
    return copy;
  }
  return [...m, { turn, role: "assistant", text: patch("") }];
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
