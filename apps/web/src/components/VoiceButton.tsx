import type { TtsState } from "../lib/ttsSettings";

export function VoiceButton({
  state,
  progress,
  onLoad,
}: {
  state: TtsState;
  progress: number;
  onLoad: () => void;
}) {
  if (state === "ready") return <span className="pill ok">🔊 음성 준비 완료</span>;
  if (state === "loading")
    return <span className="pill">음성 불러오는 중… {Math.round(progress)}%</span>;
  if (state === "error")
    return (
      <button
        type="button"
        className="pill err"
        onClick={onLoad}
        aria-label="음성 다시 불러오기"
        title="음성 다시 불러오기"
      >
        ⚠️ 음성을 불러오지 못했어요, 탭해서 다시 시도
      </button>
    );
  return (
    <button className="ghost" onClick={onLoad}>
      음성 활성화
    </button>
  );
}
