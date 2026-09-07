import type { FormEventHandler, KeyboardEventHandler, RefObject } from "react";
import { WordSearchControl } from "./WordSearchControl";

interface MessageComposerProps {
  ended: boolean;
  quickMode: boolean;
  quickSent: boolean;
  mic: boolean;
  transcribing: boolean;
  text: string;
  voiceDraft: boolean;
  textareaRef: RefObject<HTMLTextAreaElement | null>;
  onToggleMic: () => void;
  onSubmit: FormEventHandler<HTMLFormElement>;
  onKeyDown: KeyboardEventHandler<HTMLTextAreaElement>;
  onTextChange: (value: string) => void;
  onDiscardVoiceDraft: () => void;
}

// Composer presentation stays independent of microphone/WebSocket ownership.
// App supplies commands and state; this component defines the mutually
// exclusive ended, quick-waiting, and editable layouts.
export function MessageComposer({
  ended,
  quickMode,
  quickSent,
  mic,
  transcribing,
  text,
  voiceDraft,
  textareaRef,
  onToggleMic,
  onSubmit,
  onKeyDown,
  onTextChange,
  onDiscardVoiceDraft,
}: MessageComposerProps) {
  if (ended) {
    return (
      <footer className="composer composer-ended">
        <p className="hint ended" role="status">
          {quickMode
            ? "인스턴트 대화를 완료했어요! 위 🎓 버튼에서 학습 피드백을 확인해보세요."
            : "이 대화는 종료되어 더 이상 메시지를 보낼 수 없어요."}
        </p>
      </footer>
    );
  }

  if (quickMode && quickSent) {
    return (
      <footer className="composer composer-ended">
        <p className="hint ended" role="status">
          <span className="spinning">⏳</span> 답변을 기다리는 중이에요…
        </p>
      </footer>
    );
  }

  return (
    <footer className="composer">
      <WordSearchControl />
      <button
        className={`mic ${mic ? "on" : ""}`}
        onClick={onToggleMic}
        aria-label={transcribing ? "음성 인식 중" : mic ? "녹음 중지" : "음성으로 말하기"}
        aria-pressed={mic}
        title={transcribing ? "음성 인식 중" : mic ? "녹음 중지" : "음성으로 말하기"}
        disabled={transcribing}
      >
        {mic ? "◼" : "🎙"}
      </button>
      {transcribing && (
        <p className="hint transcribing" role="status" aria-label="음성 인식 중">
          <span className="spinning">⏳</span>
        </p>
      )}
      <form onSubmit={onSubmit}>
        <div className="composer-field">
          {voiceDraft && (
            <span id="voice-draft-note" className="voice-draft-note" role="status">
              음성 인식 결과예요. 확인한 뒤 보내주세요.
            </span>
          )}
          <textarea
            ref={textareaRef}
            className={voiceDraft ? "voice-draft" : undefined}
            value={text}
            onChange={(event) => onTextChange(event.target.value)}
            onKeyDown={onKeyDown}
            placeholder="…or type in English"
            aria-label="영어 메시지"
            aria-describedby={voiceDraft ? "voice-draft-note" : undefined}
            enterKeyHint="send"
            autoComplete="off"
            autoCorrect="on"
            rows={1}
          />
        </div>
        {voiceDraft && (
          <button
            type="button"
            className="ghost icon-btn"
            onClick={onDiscardVoiceDraft}
            aria-label="음성 초안 취소"
            title="음성 초안 취소"
          >
            ✕
          </button>
        )}
        <button
          type="submit"
          className="send-btn"
          aria-label="메시지 보내기"
          title="메시지 보내기"
          disabled={!text.trim()}
        >
          ➤
        </button>
      </form>
    </footer>
  );
}
