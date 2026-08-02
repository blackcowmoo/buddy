import { NATIVE_RATE } from "../lib/ttsSettings";

// StudyControl collapses per-rate playback into one small button that opens
// a popover, kept separate from the bubble/correction card so it doesn't
// compete with them for visual weight.
export function StudyControl({
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
