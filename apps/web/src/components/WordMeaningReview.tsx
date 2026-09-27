import type { MeaningChoice, WordReviewItem } from "../lib/wordReview";

export function WordMeaningReview({ word, selecting, error, onSelect }: {
  word: WordReviewItem;
  selecting: boolean;
  error?: string;
  onSelect: (choice: MeaningChoice) => void;
}) {
  return (
    <>
      {word.meaningStatus === "done" ? (
        <div className="word-meaning-choices">
          <button type="button" className="ghost word-meaning-choice" disabled={selecting} onClick={() => onSelect("cleaned")}
            aria-label={`정리 후 뜻 ${word.meaning} 이 뜻으로 확정`}>
            <span className="hint">정리 후 뜻</span>
            <span>{word.meaning}</span>
            <span>이 뜻으로 확정</span>
          </button>
          {word.previousMeaning && (
            <button type="button" className="ghost word-meaning-choice" disabled={selecting} onClick={() => onSelect("original")}
              aria-label={`정리 전 뜻 ${word.previousMeaning} 이 뜻으로 다시 정리`}>
              <span className="hint">정리 전 뜻</span>
              <span>{word.previousMeaning}</span>
              <span>이 뜻으로 다시 정리</span>
            </button>
          )}
        </div>
      ) : <span className="word-list-meaning">{word.meaning}</span>}
      {word.meaningStatus === "pending" && <span className="hint" role="status">뜻 정리 중… 완료되면 뜻을 선택해 주세요.</span>}
      {word.meaningStatus === "failed" && (
        <>
          <span className="hint">{word.meaningError}</span>
          <button type="button" className="ghost" disabled={selecting} onClick={() => onSelect("original")}>이 뜻으로 다시 정리</button>
        </>
      )}
      {selecting && <span className="hint" role="status">선택을 저장 중…</span>}
      {error && <span className="hint" role="alert">{error}</span>}
    </>
  );
}
