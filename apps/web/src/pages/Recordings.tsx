import { useEffect, useState } from "react";
import { EmptyState, LearningIntro } from "../components/LearningIntro";
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
        <LearningIntro eyebrow="내 목소리로 돌아보는 영어" title="말했던 영어를 다시 들어요" description="저장된 녹음을 들으며 발음과 말하는 리듬을 살펴보세요. 조금씩 편해지는 내 영어를 발견할 수 있어요." />
        {state === "loading" && <LoadingHint />}
        {state === "error" && (
          <p className="hint" role="alert">녹음 목록을 불러오지 못했어요. 연결 상태와 녹음 저장 설정을 확인한 뒤 다시 열어 주세요.</p>
        )}
        {state === "ready" && recordings.length === 0 && (
          <EmptyState title="아직 저장된 녹음이 없어요." description="대화에서 마이크로 말해 보세요. 녹음 저장이 켜져 있으면 이곳에서 다시 들을 수 있어요." href="." action="대화하러 가기" />
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
