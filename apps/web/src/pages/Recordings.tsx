import { useEffect, useState } from "react";
import { fetchRecordings, recordingAudioURL, type Recording } from "../lib/recordings";

type LoadState = "loading" | "ready" | "error";

export function Recordings() {
  const [state, setState] = useState<LoadState>("loading");
  const [recordings, setRecordings] = useState<Recording[]>([]);

  useEffect(() => {
    fetchRecordings().then((list) => {
      if (list === null) {
        setState("error");
        return;
      }
      setRecordings(list);
      setState("ready");
    });
  }, []);

  return (
    <div className="app">
      <header className="topbar">
        <div className="brand">
          <h1>녹음 목록</h1>
        </div>
        {/* Relative link (not "/"): resolves against the current page URL,
            same reasoning as PCMRecorder's worklet URL, so this still works
            under a ROOT_PATH prefix like "/pr/14/recordings". */}
        <a className="ghost icon-btn" href="." aria-label="대화로 돌아가기" title="대화로 돌아가기">
          ←
        </a>
      </header>

      <main className="convo recordings-list">
        {state === "loading" && <p className="hint">불러오는 중…</p>}
        {state === "error" && (
          <p className="hint">
            녹음 목록을 불러오지 못했습니다. 녹음 저장이 설정되어 있지 않거나, 네트워크 문제일 수
            있습니다.
          </p>
        )}
        {state === "ready" && recordings.length === 0 && (
          <p className="hint">아직 저장된 녹음이 없습니다.</p>
        )}
        {state === "ready" &&
          recordings.map((r) => (
            <div key={r.id} className="recording-row">
              <div className="recording-meta">
                <span className="recording-time">{formatDate(r.createdAt)}</span>
                <span className="recording-sub">
                  {formatDuration(r.durationMs)} · {formatSize(r.sizeBytes)}
                </span>
              </div>
              <audio controls preload="none" src={recordingAudioURL(r.id)} />
            </div>
          ))}
      </main>
    </div>
  );
}

function formatDate(unixSeconds: number): string {
  return new Date(unixSeconds * 1000).toLocaleString();
}

function formatDuration(ms: number): string {
  const totalSeconds = Math.round(ms / 1000);
  const m = Math.floor(totalSeconds / 60);
  const s = totalSeconds % 60;
  return `${m}:${s.toString().padStart(2, "0")}`;
}

function formatSize(bytes: number): string {
  if (bytes < 1024) return `${bytes} B`;
  const kb = bytes / 1024;
  if (kb < 1024) return `${kb.toFixed(1)} KB`;
  return `${(kb / 1024).toFixed(1)} MB`;
}
