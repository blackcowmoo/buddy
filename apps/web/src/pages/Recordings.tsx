import { useEffect, useState } from "react";
import { confirmThenDelete } from "../lib/confirmDelete";
import { deleteRecording, fetchRecordings, recordingAudioURL, type Recording } from "../lib/recordings";
import { formatAbsoluteDateTime } from "../lib/time";
import { SubPageHeader } from "../components/SubPageHeader";
import { LoadingHint } from "../components/LoadingHint";
import type { LoadState } from "../lib/loadState";

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

  const handleDelete = (id: string) => confirmThenDelete("이 녹음을 삭제할까요?", deleteRecording, id, setRecordings);

  return (
    <div className="app">
      <SubPageHeader title="녹음 목록" />

      <main className="convo recordings-list">
        {state === "loading" && <LoadingHint />}
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
              <div className="recording-header">
                <div className="recording-meta">
                  <span className="recording-time">{formatAbsoluteDateTime(r.createdAt)}</span>
                  <span className="recording-sub">
                    {formatDuration(r.durationMs)} · {formatSize(r.sizeBytes)}
                  </span>
                </div>
                <button
                  type="button"
                  className="ghost icon-btn recording-delete"
                  onClick={() => void handleDelete(r.id)}
                  aria-label="녹음 삭제"
                  title="녹음 삭제"
                >
                  🗑
                </button>
              </div>
              <audio controls preload="none" src={recordingAudioURL(r.id)} />
            </div>
          ))}
      </main>
    </div>
  );
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
